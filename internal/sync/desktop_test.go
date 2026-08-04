package sync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tawanorg/claude-sync/internal/config"
)

// writeDesktopFixture creates a claude-code-sessions/<account>/<install>/
// pointer directory under dir and writes the given local_*.json files into
// it, returning that pointer directory's path.
func writeDesktopFixture(t *testing.T, dir string, pointers map[string]map[string]interface{}) string {
	t.Helper()
	ptrDir := filepath.Join(dir, "claude-code-sessions", "acct-1", "install-1")
	if err := os.MkdirAll(ptrDir, 0755); err != nil {
		t.Fatal(err)
	}
	for filename, fields := range pointers {
		data, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ptrDir, filename), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return ptrDir
}

// writeSessionTranscript creates a minimal ~/.claude/projects/<enc>/<id>.jsonl
// so a cliSessionId counts as "synced" from this device's point of view.
func writeSessionTranscript(t *testing.T, claudeDir, encodedProject, cliSessionID string) {
	t.Helper()
	dir := filepath.Join(claudeDir, "projects", encodedProject)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, cliSessionID+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPushDesktopSessions_SkipsUnsyncedCLISessionID(t *testing.T) {
	syncer, _, claudeDir := testSyncer(t)
	ctx := context.Background()

	appData := t.TempDir()
	syncer.cfg.DesktopAppDataOverride = appData
	writeDesktopFixture(t, appData, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":    "local_aaa",
			"cliSessionId": "no-transcript-for-this-one",
			"cwd":          filepath.Join(claudeDir, "..", "my-app"),
			"title":        "Orphaned",
		},
	})

	result, err := syncer.PushDesktopSessions(ctx)
	if err != nil {
		t.Fatalf("PushDesktopSessions: %v", err)
	}
	if len(result.Pushed) != 0 {
		t.Errorf("expected nothing pushed, got %v", result.Pushed)
	}
	if result.Skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", result.Skipped)
	}
}

func TestPushDesktopSessions_PushesSyncedSession(t *testing.T) {
	syncer, store, claudeDir := testSyncer(t)
	syncer.paths = mustMapper(t, "/Users/alice", nil)
	ctx := context.Background()

	writeSessionTranscript(t, claudeDir, "-Users-alice-my-app", "cli-123")

	appData := t.TempDir()
	syncer.cfg.DesktopAppDataOverride = appData
	writeDesktopFixture(t, appData, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":    "local_aaa",
			"cliSessionId": "cli-123",
			"cwd":          "/Users/alice/my-app",
			"title":        "My App Work",
			"model":        "claude-sonnet-5",
		},
	})

	result, err := syncer.PushDesktopSessions(ctx)
	if err != nil {
		t.Fatalf("PushDesktopSessions: %v", err)
	}
	if len(result.Pushed) != 1 || result.Pushed[0] != "cli-123" {
		t.Fatalf("expected [cli-123] pushed, got %v", result.Pushed)
	}

	remoteKey := DesktopSessionsRemoteKeyPrefix + "cli-123.json.age"
	if _, err := store.Download(ctx, remoteKey); err != nil {
		t.Fatalf("expected remote object at %s: %v", remoteKey, err)
	}

	// Pushing again with no changes should report Unchanged, not re-push.
	result2, err := syncer.PushDesktopSessions(ctx)
	if err != nil {
		t.Fatalf("second PushDesktopSessions: %v", err)
	}
	if len(result2.Pushed) != 0 || result2.Unchanged != 1 {
		t.Errorf("expected no-op second push, got Pushed=%v Unchanged=%d", result2.Pushed, result2.Unchanged)
	}
}

func TestPushDesktopSessions_NormalizesCWD(t *testing.T) {
	syncer, store, claudeDir := testSyncer(t)
	syncer.paths = mustMapper(t, "/Users/alice", nil)
	ctx := context.Background()

	writeSessionTranscript(t, claudeDir, "-Users-alice-my-app", "cli-123")

	appData := t.TempDir()
	syncer.cfg.DesktopAppDataOverride = appData
	writeDesktopFixture(t, appData, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":    "local_aaa",
			"cliSessionId": "cli-123",
			"cwd":          "/Users/alice/my-app",
		},
	})

	if _, err := syncer.PushDesktopSessions(ctx); err != nil {
		t.Fatal(err)
	}

	encrypted, err := store.Download(ctx, DesktopSessionsRemoteKeyPrefix+"cli-123.json.age")
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := syncer.encryptor.Decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	data, err := gzipDecompress(compressed)
	if err != nil {
		t.Fatal(err)
	}
	var rec DesktopSessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.CWD != "${HOME}/my-app" {
		t.Errorf("expected portable cwd ${HOME}/my-app, got %q", rec.CWD)
	}
}

// TestDesktopSessionsRoundTrip covers the core cross-device scenario: device
// A has a Desktop pointer for a session; device B already has the transcript
// synced but has never opened that session in Desktop. After push (A) and
// pull (B), device B should get a brand-new local pointer with its own
// device-local sessionId, the transcript's title carried over, and cwd
// resolved to B's own path.
func TestDesktopSessionsRoundTrip(t *testing.T) {
	syncerA, store, claudeDirA := testSyncer(t)
	syncerA.paths = mustMapper(t, "/Users/alice", nil)
	ctx := context.Background()

	writeSessionTranscript(t, claudeDirA, "-Users-alice-my-app", "cli-123")
	appDataA := t.TempDir()
	syncerA.cfg.DesktopAppDataOverride = appDataA
	writeDesktopFixture(t, appDataA, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":      "local_aaa",
			"cliSessionId":   "cli-123",
			"cwd":            "/Users/alice/my-app",
			"title":          "My App Work",
			"model":          "claude-sonnet-5",
			"completedTurns": 42,
		},
	})
	if _, err := syncerA.PushDesktopSessions(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Device B: same transcript already synced (as regular push/pull would
	// have done), but Desktop has never been opened there for this project —
	// no existing pointer directory at all.
	tmpB := t.TempDir()
	claudeDirB := filepath.Join(tmpB, ".claude")
	if err := os.MkdirAll(claudeDirB, 0755); err != nil {
		t.Fatal(err)
	}
	stateB, err := LoadStateFromDir(tmpB)
	if err != nil {
		t.Fatal(err)
	}
	syncerB := NewSyncerWith(&config.Config{}, store, syncerA.encryptor, stateB, claudeDirB, true)
	syncerB.paths = mustMapper(t, "/Users/bob", nil)
	writeSessionTranscript(t, claudeDirB, "-Users-bob-my-app", "cli-123")
	appDataB := t.TempDir()
	syncerB.cfg.DesktopAppDataOverride = appDataB
	ptrDirB := writeDesktopFixture(t, appDataB, map[string]map[string]interface{}{}) // creates the dir, no pointers yet

	result, err := syncerB.PullDesktopSessions(ctx)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(result.Created) != 1 || result.Created[0] != "cli-123" {
		t.Fatalf("expected [cli-123] created, got %v", result.Created)
	}

	entries, err := os.ReadDir(ptrDirB)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 pointer file created, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(ptrDirB, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["cliSessionId"] != "cli-123" {
		t.Errorf("cliSessionId = %v, want cli-123", raw["cliSessionId"])
	}
	if raw["cwd"] != "/Users/bob/my-app" {
		t.Errorf("cwd = %v, want /Users/bob/my-app (resolved to device B's path)", raw["cwd"])
	}
	if raw["title"] != "My App Work" {
		t.Errorf("title = %v, want carried-over title", raw["title"])
	}
	sid, _ := raw["sessionId"].(string)
	if sid == "local_aaa" || sid == "" {
		t.Errorf("sessionId = %q, want a freshly generated device-local id, not device A's", sid)
	}
}

func TestPullDesktopSessions_UpdatesExistingPointerInPlace(t *testing.T) {
	syncerA, store, claudeDirA := testSyncer(t)
	syncerA.paths = mustMapper(t, "/Users/alice", nil)
	ctx := context.Background()

	writeSessionTranscript(t, claudeDirA, "-Users-alice-my-app", "cli-123")
	appDataA := t.TempDir()
	syncerA.cfg.DesktopAppDataOverride = appDataA
	writeDesktopFixture(t, appDataA, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":      "local_aaa",
			"cliSessionId":   "cli-123",
			"cwd":            "/Users/alice/my-app",
			"title":          "Updated Title",
			"completedTurns": 99,
		},
	})
	if _, err := syncerA.PushDesktopSessions(ctx); err != nil {
		t.Fatal(err)
	}

	// Device B already has its OWN pointer for this cliSessionId (it opened
	// the session locally too, at some point) with an old title/turn count,
	// and its own distinct device-local sessionId that must survive the pull.
	tmpB := t.TempDir()
	claudeDirB := filepath.Join(tmpB, ".claude")
	os.MkdirAll(claudeDirB, 0755)
	stateB, _ := LoadStateFromDir(tmpB)
	syncerB := NewSyncerWith(&config.Config{}, store, syncerA.encryptor, stateB, claudeDirB, true)
	syncerB.paths = mustMapper(t, "/Users/bob", nil)
	writeSessionTranscript(t, claudeDirB, "-Users-bob-my-app", "cli-123")
	appDataB := t.TempDir()
	syncerB.cfg.DesktopAppDataOverride = appDataB
	ptrDirB := writeDesktopFixture(t, appDataB, map[string]map[string]interface{}{
		"local_bbb.json": {
			"sessionId":      "local_bbb",
			"cliSessionId":   "cli-123",
			"cwd":            "/Users/bob/my-app",
			"title":          "Stale Title",
			"completedTurns": 3,
		},
	})
	// Back-date the pointer so it falls outside the "recently active, don't
	// touch" freshness window this pull would otherwise respect.
	old := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(filepath.Join(ptrDirB, "local_bbb.json"), old, old); err != nil {
		t.Fatal(err)
	}

	result, err := syncerB.PullDesktopSessions(ctx)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(result.Updated) != 1 || result.Updated[0] != "cli-123" {
		t.Fatalf("expected [cli-123] updated, got %v", result)
	}

	data, err := os.ReadFile(filepath.Join(ptrDirB, "local_bbb.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if raw["sessionId"] != "local_bbb" {
		t.Errorf("sessionId changed to %v, want it preserved as local_bbb", raw["sessionId"])
	}
	if raw["title"] != "Updated Title" {
		t.Errorf("title = %v, want refreshed to 'Updated Title'", raw["title"])
	}
	if int(raw["completedTurns"].(float64)) != 99 {
		t.Errorf("completedTurns = %v, want refreshed to 99", raw["completedTurns"])
	}
}

func TestPullDesktopSessions_SkipsRecentlyActivePointer(t *testing.T) {
	syncerA, store, claudeDirA := testSyncer(t)
	syncerA.paths = mustMapper(t, "/Users/alice", nil)
	ctx := context.Background()

	writeSessionTranscript(t, claudeDirA, "-Users-alice-my-app", "cli-123")
	appDataA := t.TempDir()
	syncerA.cfg.DesktopAppDataOverride = appDataA
	writeDesktopFixture(t, appDataA, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":    "local_aaa",
			"cliSessionId": "cli-123",
			"cwd":          "/Users/alice/my-app",
			"title":        "From A",
		},
	})
	if _, err := syncerA.PushDesktopSessions(ctx); err != nil {
		t.Fatal(err)
	}

	tmpB := t.TempDir()
	claudeDirB := filepath.Join(tmpB, ".claude")
	os.MkdirAll(claudeDirB, 0755)
	stateB, _ := LoadStateFromDir(tmpB)
	syncerB := NewSyncerWith(&config.Config{}, store, syncerA.encryptor, stateB, claudeDirB, true)
	syncerB.paths = mustMapper(t, "/Users/bob", nil)
	writeSessionTranscript(t, claudeDirB, "-Users-bob-my-app", "cli-123")
	appDataB := t.TempDir()
	syncerB.cfg.DesktopAppDataOverride = appDataB
	// Freshly written (mtime = now): looks like Desktop has this open on B right now.
	ptrDirB := writeDesktopFixture(t, appDataB, map[string]map[string]interface{}{
		"local_bbb.json": {
			"sessionId":    "local_bbb",
			"cliSessionId": "cli-123",
			"cwd":          "/Users/bob/my-app",
			"title":        "Locally active on B",
		},
	})

	result, err := syncerB.PullDesktopSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Updated) != 0 {
		t.Errorf("expected no update to a freshly-modified pointer, got %v", result.Updated)
	}
	if len(result.Skipped) != 1 {
		t.Errorf("expected 1 skipped, got %v", result.Skipped)
	}

	data, _ := os.ReadFile(filepath.Join(ptrDirB, "local_bbb.json"))
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if raw["title"] != "Locally active on B" {
		t.Errorf("pointer was overwritten despite being recently modified: title = %v", raw["title"])
	}
}

func TestPullDesktopSessions_SkipsWhenNoLocalTranscript(t *testing.T) {
	syncerA, store, claudeDirA := testSyncer(t)
	syncerA.paths = mustMapper(t, "/Users/alice", nil)
	ctx := context.Background()

	writeSessionTranscript(t, claudeDirA, "-Users-alice-my-app", "cli-123")
	appDataA := t.TempDir()
	syncerA.cfg.DesktopAppDataOverride = appDataA
	writeDesktopFixture(t, appDataA, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":    "local_aaa",
			"cliSessionId": "cli-123",
			"cwd":          "/Users/alice/my-app",
		},
	})
	if _, err := syncerA.PushDesktopSessions(ctx); err != nil {
		t.Fatal(err)
	}

	// Device B has never synced this project at all — no transcript locally.
	tmpB := t.TempDir()
	claudeDirB := filepath.Join(tmpB, ".claude")
	os.MkdirAll(claudeDirB, 0755)
	stateB, _ := LoadStateFromDir(tmpB)
	syncerB := NewSyncerWith(&config.Config{}, store, syncerA.encryptor, stateB, claudeDirB, true)
	syncerB.paths = mustMapper(t, "/Users/bob", nil)
	appDataB := t.TempDir()
	syncerB.cfg.DesktopAppDataOverride = appDataB
	writeDesktopFixture(t, appDataB, map[string]map[string]interface{}{})

	result, err := syncerB.PullDesktopSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 0 || len(result.Updated) != 0 {
		t.Errorf("expected nothing created/updated without a local transcript, got %+v", result)
	}
	if len(result.Skipped) != 1 {
		t.Errorf("expected 1 skipped, got %v", result.Skipped)
	}
}

func TestFindDesktopPointers_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	writeDesktopFixture(t, dir, map[string]map[string]interface{}{
		"local_a.json": {"sessionId": "local_a", "cliSessionId": "cli-1"},
		"local_b.json": {"sessionId": "local_b", "cliSessionId": "cli-2"},
		"not-a-pointer.json": {"foo": "bar"},
	})

	pointers, err := findDesktopPointers(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pointers) != 2 {
		t.Fatalf("expected 2 pointers, got %d", len(pointers))
	}
	if _, ok := pointers["cli-1"]; !ok {
		t.Error("missing cli-1")
	}
	if _, ok := pointers["cli-2"]; !ok {
		t.Error("missing cli-2")
	}
}

// TestRegularPushDoesNotDeleteDesktopSessionRecords guards against a real bug
// found during manual testing: PushDesktopSessions records its remote key
// (_external/desktop-sessions/<id>.json) in the shared state.Files map for
// change-detection, exactly like PushMCP does for _external/mcp-servers.json.
// Without a guard in DetectChanges, a regular Push() sees that synthetic path
// in state.Files, can never find it via the normal filesystem walk (it was
// never a real file under claudeDir), concludes it was "deleted locally", and
// wipes the remote record it just wrote — on every subsequent Push().
func TestRegularPushDoesNotDeleteDesktopSessionRecords(t *testing.T) {
	syncer, store, claudeDir := testSyncer(t)
	syncer.paths = mustMapper(t, "/Users/alice", nil)
	ctx := context.Background()

	writeSessionTranscript(t, claudeDir, "-Users-alice-my-app", "cli-123")
	appData := t.TempDir()
	syncer.cfg.DesktopAppDataOverride = appData
	writeDesktopFixture(t, appData, map[string]map[string]interface{}{
		"local_aaa.json": {
			"sessionId":    "local_aaa",
			"cliSessionId": "cli-123",
			"cwd":          "/Users/alice/my-app",
			"title":        "My App Work",
		},
	})

	if _, err := syncer.PushDesktopSessions(ctx); err != nil {
		t.Fatalf("PushDesktopSessions: %v", err)
	}
	remoteKey := DesktopSessionsRemoteKeyPrefix + "cli-123.json.age"
	if _, err := store.Download(ctx, remoteKey); err != nil {
		t.Fatalf("record missing right after push: %v", err)
	}

	// A regular Push (e.g. the next scheduled sync, unrelated to Desktop
	// sessions at all) must not touch it.
	if _, err := syncer.Push(ctx); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if _, err := store.Download(ctx, remoteKey); err != nil {
		t.Fatalf("regular Push deleted the desktop-session record: %v", err)
	}
}

func TestDesktopAppDataDir_UsesOverride(t *testing.T) {
	syncer, _, _ := testSyncer(t)
	syncer.cfg.DesktopAppDataOverride = "/custom/path"
	got, err := syncer.desktopAppDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/path" {
		t.Errorf("got %q, want /custom/path", got)
	}
}
