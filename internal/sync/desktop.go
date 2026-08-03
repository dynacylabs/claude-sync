package sync

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// Claude Desktop keeps its own record of which Code-tab chat entries exist,
// entirely separate from the ~/.claude/projects/*.jsonl transcripts that
// `claude --resume` reads. Each entry is a small JSON file:
//
//	~/.config/Claude/claude-code-sessions/<account-uuid>/<install-uuid>/local_<uuid>.json
//	  (%APPDATA%\Claude\... on Windows, ~/Library/Application Support/Claude/... on macOS)
//
// The file's "cliSessionId" field is the join key back to the portable
// transcript (<cliSessionId>.jsonl) that regular claude-sync push/pull already
// moves between devices. Desktop's own session list/"Recents" UI is driven by
// these pointer files, not by scanning ~/.claude/projects directly, so syncing
// the transcript alone is not enough to make a session appear in Desktop's GUI
// on another device — this file syncs the pointers too.
//
// "sessionId" (the local_<uuid> value) is device-local identity, not portable:
// each device gets its own freshly generated one. Only cliSessionId and the
// display metadata (title, cwd, timestamps, ...) travel across devices.

// desktopSessionDirName is the fixed directory Claude Desktop stores session
// pointers under, relative to its per-OS app-data directory.
const desktopSessionDirName = "claude-code-sessions"

// DesktopSessionsRemoteKeyPrefix namespaces synced pointer records in remote
// storage, one object per cliSessionId. Like MCPRemoteKey, the _external/
// prefix keeps these out of the generic ~/.claude-relative file walk; Push and
// Pull handle this prefix explicitly instead.
const DesktopSessionsRemoteKeyPrefix = "_external/desktop-sessions/"

// desktopPointerFreshWindow guards against clobbering a pointer file Desktop
// itself is actively writing right now (an open window on this device). A
// pointer modified more recently than this is left alone on pull.
const desktopPointerFreshWindow = 5 * time.Minute

// DesktopSessionRecord is the portable, cross-device projection of a Desktop
// session pointer: the subset of fields worth showing in another device's
// Desktop GUI. Deliberately excludes fields that are large, device-specific,
// or not needed for the entry to render, such as alwaysAllowedReasons (a
// per-device tool-approval cache that can run to hundreds of KB).
type DesktopSessionRecord struct {
	CLISessionID    string   `json:"cliSessionId"`
	CWD             string   `json:"cwd"` // portable form, e.g. ${HOME}/claude/masha
	Title           string   `json:"title,omitempty"`
	TitleSource     string   `json:"titleSource,omitempty"`
	Model           string   `json:"model,omitempty"`
	Effort          string   `json:"effort,omitempty"`
	IsArchived      bool     `json:"isArchived,omitempty"`
	PermissionMode  string   `json:"permissionMode,omitempty"`
	WrittenBranches []string `json:"writtenBranches,omitempty"`
	CompletedTurns  int      `json:"completedTurns,omitempty"`
	CreatedAt       int64    `json:"createdAt,omitempty"`
	LastActivityAt  int64    `json:"lastActivityAt,omitempty"`
	LastFocusedAt   int64    `json:"lastFocusedAt,omitempty"`
}

// DesktopSessionsPushResult describes the outcome of pushing Desktop session pointers.
type DesktopSessionsPushResult struct {
	Pushed    []string // cliSessionIds pushed
	Unchanged int
	Skipped   int // pointers whose cliSessionId isn't in our synced project set
}

// DesktopSessionsPullResult describes the outcome of pulling Desktop session pointers.
type DesktopSessionsPullResult struct {
	Created []string // cliSessionIds that got a new local pointer file
	Updated []string // cliSessionIds whose existing local pointer was refreshed
	Skipped []string // cliSessionIds skipped: no local transcript, or pointer looks actively in use
}

// desktopAppDataDir returns Claude Desktop's per-OS application data
// directory, or an error if it can't be determined on this platform. Honors
// cfg.DesktopAppDataOverride so tests never touch the real Desktop install.
func (s *Syncer) desktopAppDataDir() (string, error) {
	if s.cfg.DesktopAppDataOverride != "" {
		return s.cfg.DesktopAppDataOverride, nil
	}
	return defaultDesktopAppDataDir()
}

func defaultDesktopAppDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrNoHomeDir
	}

	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Claude"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude"), nil
	default: // linux and other unix-likes
		configHome := os.Getenv("XDG_CONFIG_HOME")
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		return filepath.Join(configHome, "Claude"), nil
	}
}

// ErrNoHomeDir mirrors config.ErrNoHomeDir for callers in this package that
// don't want to import config just for the sentinel.
var ErrNoHomeDir = config.ErrNoHomeDir

// desktopPointerDirs enumerates every claude-code-sessions/<account>/<install>
// directory found under the Desktop app-data directory. There is normally
// exactly one (account, install) pair, but a reinstalled or multi-account
// setup can leave more than one; all are scanned so no pointer is missed.
func desktopPointerDirs(appDataDir string) ([]string, error) {
	root := filepath.Join(appDataDir, desktopSessionDirName)
	accountDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", root, err)
	}

	var dirs []string
	for _, ad := range accountDirs {
		if !ad.IsDir() {
			continue
		}
		accountPath := filepath.Join(root, ad.Name())
		installDirs, err := os.ReadDir(accountPath)
		if err != nil {
			continue
		}
		for _, id := range installDirs {
			if id.IsDir() {
				dirs = append(dirs, filepath.Join(accountPath, id.Name()))
			}
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// desktopPointer holds a parsed local_*.json pointer file: its full field set
// (as a generic map, so unknown/unmodeled fields survive a read-modify-write)
// plus the handful of fields this package cares about.
type desktopPointer struct {
	path         string
	raw          map[string]json.RawMessage
	sessionID    string
	cliSessionID string
	modTime      time.Time
}

func readDesktopPointer(path string, info os.FileInfo) (*desktopPointer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	p := &desktopPointer{path: path, raw: raw, modTime: info.ModTime()}
	if v, ok := raw["sessionId"]; ok {
		_ = json.Unmarshal(v, &p.sessionID)
	}
	if v, ok := raw["cliSessionId"]; ok {
		_ = json.Unmarshal(v, &p.cliSessionID)
	}
	return p, nil
}

// findDesktopPointers scans every discovered pointer directory and returns
// all local_*.json pointers found, keyed by cliSessionId. When more than one
// pointer exists for the same cliSessionId (unexpected, but not impossible
// after a reinstall) the most recently modified one wins.
func findDesktopPointers(appDataDir string) (map[string]*desktopPointer, error) {
	dirs, err := desktopPointerDirs(appDataDir)
	if err != nil {
		return nil, err
	}

	byCLISessionID := make(map[string]*desktopPointer)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			ptr, err := readDesktopPointer(filepath.Join(dir, name), info)
			if err != nil || ptr.cliSessionID == "" {
				continue
			}
			if existing, ok := byCLISessionID[ptr.cliSessionID]; !ok || ptr.modTime.After(existing.modTime) {
				byCLISessionID[ptr.cliSessionID] = ptr
			}
		}
	}
	return byCLISessionID, nil
}

// syncedCLISessionIDs returns the set of session ids (jsonl basenames,
// without extension) present under ~/.claude/projects and not excluded, i.e.
// the transcripts this device actually has via the regular file sync. A
// Desktop pointer is only worth pushing or pulling when its cliSessionId is
// in this set — otherwise it points at a transcript the other device has no
// way to open.
func (s *Syncer) syncedCLISessionIDs() (map[string]bool, error) {
	ids := make(map[string]bool)
	projectsDir := filepath.Join(s.claudeDir, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ids, nil
		}
		return nil, fmt.Errorf("failed to read %s: %w", projectsDir, err)
	}
	for _, projectDir := range entries {
		if !projectDir.IsDir() {
			continue
		}
		relDir := filepath.ToSlash(filepath.Join("projects", projectDir.Name()))
		if s.isExcluded(relDir) {
			continue
		}
		sessionFiles, err := os.ReadDir(filepath.Join(projectsDir, projectDir.Name()))
		if err != nil {
			continue
		}
		for _, f := range sessionFiles {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			relFile := relDir + "/" + name
			if s.isExcluded(relFile) {
				continue
			}
			ids[strings.TrimSuffix(name, ".jsonl")] = true
		}
	}
	return ids, nil
}

func newDesktopSessionUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// RFC 4122 version 4 / variant bits, matching the local_<uuid> form Desktop itself writes.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func desktopRecordFromPointer(p *desktopPointer) DesktopSessionRecord {
	get := func(key string, dst interface{}) {
		if v, ok := p.raw[key]; ok {
			_ = json.Unmarshal(v, dst)
		}
	}
	rec := DesktopSessionRecord{CLISessionID: p.cliSessionID}
	get("cwd", &rec.CWD)
	get("title", &rec.Title)
	get("titleSource", &rec.TitleSource)
	get("model", &rec.Model)
	get("effort", &rec.Effort)
	get("isArchived", &rec.IsArchived)
	get("permissionMode", &rec.PermissionMode)
	get("writtenBranches", &rec.WrittenBranches)
	get("completedTurns", &rec.CompletedTurns)
	get("createdAt", &rec.CreatedAt)
	get("lastActivityAt", &rec.LastActivityAt)
	get("lastFocusedAt", &rec.LastFocusedAt)
	return rec
}

// PushDesktopSessions uploads a portable record for every local Desktop
// session pointer whose cliSessionId corresponds to a transcript this device
// already syncs. One remote object per cliSessionId; last push wins, since
// these are lightweight display metadata, not the conversation itself (the
// transcript keeps its own conflict handling via the regular file sync).
func (s *Syncer) PushDesktopSessions(ctx context.Context) (*DesktopSessionsPushResult, error) {
	result := &DesktopSessionsPushResult{}

	appDataDir, err := s.desktopAppDataDir()
	if err != nil {
		return nil, err
	}
	pointers, err := findDesktopPointers(appDataDir)
	if err != nil {
		return nil, err
	}
	if len(pointers) == 0 {
		return result, nil
	}

	syncedIDs, err := s.syncedCLISessionIDs()
	if err != nil {
		return nil, err
	}

	for cliSessionID, ptr := range pointers {
		if !syncedIDs[cliSessionID] {
			result.Skipped++
			continue
		}

		rec := desktopRecordFromPointer(ptr)
		rec.CWD = string(s.paths.NormalizeContent([]byte(rec.CWD)))

		data, err := json.Marshal(rec)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize desktop session %s: %w", cliSessionID, err)
		}

		statePath := DesktopSessionsRemoteKeyPrefix + cliSessionID + ".json"
		hash := hashBytes(data)
		if existing := s.state.GetFile(statePath); existing != nil && existing.Hash == hash {
			result.Unchanged++
			continue
		}

		if err := s.uploadDesktopRecord(ctx, cliSessionID, data); err != nil {
			return nil, err
		}
		s.state.mu.Lock()
		s.state.Files[statePath] = &FileState{
			Path:     statePath,
			Hash:     hash,
			Size:     int64(len(data)),
			ModTime:  time.Now(),
			Uploaded: time.Now(),
		}
		s.state.mu.Unlock()
		result.Pushed = append(result.Pushed, cliSessionID)
	}

	if len(result.Pushed) > 0 {
		if err := s.state.Save(); err != nil {
			return result, fmt.Errorf("failed to save state: %w", err)
		}
	}

	return result, nil
}

func (s *Syncer) uploadDesktopRecord(ctx context.Context, cliSessionID string, data []byte) error {
	compressed, err := gzipCompress(data)
	if err != nil {
		return fmt.Errorf("failed to compress desktop session %s: %w", cliSessionID, err)
	}
	encrypted, err := s.encryptor.Encrypt(compressed)
	if err != nil {
		return fmt.Errorf("failed to encrypt desktop session %s: %w", cliSessionID, err)
	}
	remoteKey := DesktopSessionsRemoteKeyPrefix + cliSessionID + ".json.age"
	if err := s.storage.Upload(ctx, remoteKey, encrypted); err != nil {
		return fmt.Errorf("failed to upload desktop session %s: %w", cliSessionID, err)
	}
	return nil
}

// PullDesktopSessions downloads Desktop session pointer records and ensures
// this device has a matching local pointer for each one whose transcript it
// already has synced, so the session shows up in Desktop's own Code-tab
// session list. Existing pointers are refreshed in place (title, cwd,
// timestamps) without touching their device-local sessionId; a pointer
// modified very recently is left alone, since that most likely means Desktop
// has it open right now on this device.
func (s *Syncer) PullDesktopSessions(ctx context.Context) (*DesktopSessionsPullResult, error) {
	result := &DesktopSessionsPullResult{}

	objects, err := s.storage.List(ctx, DesktopSessionsRemoteKeyPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list desktop sessions: %w", err)
	}
	if len(objects) == 0 {
		return result, nil
	}

	syncedIDs, err := s.syncedCLISessionIDs()
	if err != nil {
		return nil, err
	}

	appDataDir, err := s.desktopAppDataDir()
	if err != nil {
		return nil, err
	}
	pointerDirs, err := desktopPointerDirs(appDataDir)
	if err != nil {
		return nil, err
	}
	existing, err := findDesktopPointers(appDataDir)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	for _, obj := range objects {
		if !strings.HasSuffix(obj.Key, ".json.age") {
			continue
		}
		cliSessionID := strings.TrimSuffix(strings.TrimPrefix(obj.Key, DesktopSessionsRemoteKeyPrefix), ".json.age")
		if cliSessionID == "" || !syncedIDs[cliSessionID] {
			result.Skipped = append(result.Skipped, cliSessionID)
			continue
		}

		rec, err := s.downloadDesktopRecord(ctx, obj.Key)
		if err != nil {
			return nil, err
		}
		rec.CWD = string(s.paths.ResolveContent([]byte(rec.CWD)))

		if ptr, ok := existing[cliSessionID]; ok {
			if now.Sub(ptr.modTime) < desktopPointerFreshWindow {
				result.Skipped = append(result.Skipped, cliSessionID)
				continue
			}
			if err := writeDesktopPointerUpdate(ptr, rec); err != nil {
				return nil, err
			}
			result.Updated = append(result.Updated, cliSessionID)
			continue
		}

		if len(pointerDirs) == 0 {
			// No Desktop installation footprint on this device at all
			// (never launched Code tab here) — nothing to attach a pointer to.
			result.Skipped = append(result.Skipped, cliSessionID)
			continue
		}
		if err := createDesktopPointer(pointerDirs[0], rec); err != nil {
			return nil, err
		}
		result.Created = append(result.Created, cliSessionID)
	}

	return result, nil
}

func (s *Syncer) downloadDesktopRecord(ctx context.Context, remoteKey string) (DesktopSessionRecord, error) {
	var rec DesktopSessionRecord
	encrypted, err := s.storage.Download(ctx, remoteKey)
	if err != nil {
		return rec, fmt.Errorf("failed to download %s: %w", remoteKey, err)
	}
	data, err := s.encryptor.Decrypt(encrypted)
	if err != nil {
		return rec, fmt.Errorf("failed to decrypt %s: %w", remoteKey, err)
	}
	if isGzipped(data) {
		data, err = gzipDecompress(data)
		if err != nil {
			return rec, fmt.Errorf("failed to decompress %s: %w", remoteKey, err)
		}
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("failed to parse %s: %w", remoteKey, err)
	}
	return rec, nil
}

// writeDesktopPointerUpdate overlays a record's fields onto an existing local
// pointer file, preserving sessionId and any field this package doesn't
// model (e.g. alwaysAllowedReasons, remoteMcpServersConfig).
func writeDesktopPointerUpdate(ptr *desktopPointer, rec DesktopSessionRecord) error {
	set := func(key string, v interface{}) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		ptr.raw[key] = data
		return nil
	}

	fields := map[string]interface{}{
		"cwd":             rec.CWD,
		"originCwd":       rec.CWD,
		"title":           rec.Title,
		"titleSource":     rec.TitleSource,
		"model":           rec.Model,
		"effort":          rec.Effort,
		"isArchived":      rec.IsArchived,
		"permissionMode":  rec.PermissionMode,
		"writtenBranches": rec.WrittenBranches,
		"completedTurns":  rec.CompletedTurns,
		"lastActivityAt":  rec.LastActivityAt,
		"lastFocusedAt":   rec.LastFocusedAt,
	}
	for k, v := range fields {
		if err := set(k, v); err != nil {
			return fmt.Errorf("failed to update %s in %s: %w", k, ptr.path, err)
		}
	}

	data, err := json.Marshal(ptr.raw)
	if err != nil {
		return fmt.Errorf("failed to serialize %s: %w", ptr.path, err)
	}
	if err := os.WriteFile(ptr.path, data, 0600); err != nil {
		return fmt.Errorf("failed to write %s: %w", ptr.path, err)
	}
	return nil
}

// createDesktopPointer writes a brand-new local_<uuid>.json pointer in dir
// for a session this device has never opened in Desktop before.
func createDesktopPointer(dir string, rec DesktopSessionRecord) error {
	newID, err := newDesktopSessionUUID()
	if err != nil {
		return fmt.Errorf("failed to generate session id: %w", err)
	}
	sessionID := "local_" + newID

	now := time.Now().UnixMilli()
	createdAt := rec.CreatedAt
	if createdAt == 0 {
		createdAt = now
	}

	full := map[string]interface{}{
		"sessionId":       sessionID,
		"cliSessionId":    rec.CLISessionID,
		"cwd":             rec.CWD,
		"originCwd":       rec.CWD,
		"createdAt":       createdAt,
		"lastActivityAt":  rec.LastActivityAt,
		"lastFocusedAt":   rec.LastFocusedAt,
		"model":           rec.Model,
		"effort":          rec.Effort,
		"isArchived":      rec.IsArchived,
		"title":           rec.Title,
		"titleSource":     rec.TitleSource,
		"permissionMode":  rec.PermissionMode,
		"writtenBranches": rec.WrittenBranches,
		"completedTurns":  rec.CompletedTurns,
	}

	data, err := json.Marshal(full)
	if err != nil {
		return fmt.Errorf("failed to serialize new desktop pointer: %w", err)
	}
	path := filepath.Join(dir, sessionID+".json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}
