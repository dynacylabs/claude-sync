package sync

import (
	"context"
	"testing"
)

func TestListCCDSessions_ReturnsAllRecordsNewestFirst(t *testing.T) {
	store := newMockStorage()
	machine, ccdDir := newCCDMachine(t, store, "install-aaa")
	ctx := context.Background()

	writeCCDRecord(t, ccdDir, "install-aaa", "local_old.json", ccdRecord("local_old", "Older", 1000))
	writeCCDRecord(t, ccdDir, "install-aaa", "local_new.json", ccdRecord("local_new", "Newer", 2000))
	writeFile(t, machine.claudeDir, "CLAUDE.md", "# a")
	if _, err := machine.syncer.Push(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}

	records, err := machine.syncer.ListCCDSessions(ctx)
	if err != nil {
		t.Fatalf("ListCCDSessions: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].Title != "Newer" || records[1].Title != "Older" {
		t.Errorf("order = [%s, %s], want [Newer, Older]", records[0].Title, records[1].Title)
	}
	if records[0].CLISessionID == "" {
		t.Error("CLISessionID should be populated from the record")
	}
}

func TestListCCDSessions_EmptyWhenNoRecords(t *testing.T) {
	store := newMockStorage()
	machine, _ := newCCDMachine(t, store, "install-aaa")
	ctx := context.Background()

	records, err := machine.syncer.ListCCDSessions(ctx)
	if err != nil {
		t.Fatalf("ListCCDSessions: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("got %d records, want 0", len(records))
	}
}

// A record ListCCDSessions surfaces must carry an id that ForgetCCDSession
// can actually match — the two are meant to be used together.
func TestListCCDSessions_IDsRoundTripThroughForget(t *testing.T) {
	store := newMockStorage()
	machine, ccdDir := newCCDMachine(t, store, "install-aaa")
	ctx := context.Background()

	writeCCDRecord(t, ccdDir, "install-aaa", "local_abc.json", ccdRecord("local_abc", "Target", 1000))
	writeFile(t, machine.claudeDir, "CLAUDE.md", "# a")
	if _, err := machine.syncer.Push(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}

	records, err := machine.syncer.ListCCDSessions(ctx)
	if err != nil {
		t.Fatalf("ListCCDSessions: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}

	result, err := machine.syncer.ForgetCCDSession(ctx, records[0].SessionID)
	if err != nil {
		t.Fatalf("ForgetCCDSession: %v", err)
	}
	if len(result.RemovedRemoteKeys) != 1 {
		t.Errorf("forget using listed sessionId removed %d records, want 1", len(result.RemovedRemoteKeys))
	}
}
