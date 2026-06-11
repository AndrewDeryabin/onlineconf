//go:build integration

package integration

import (
	"strings"
	"testing"
)

func requireLog(t *testing.T, path string, want int) []logEntry {
	t.Helper()
	log := getLog(t, path)
	if len(log) != want {
		t.Fatalf("log of %s: got %d entries %+v, want %d", path, len(log), log, want)
	}
	for _, e := range log {
		if e.Path != path {
			t.Errorf("log of %s: entry v%d is labeled %q", path, e.Version, e.Path)
		}
	}
	return log
}

// TestMoveHistoricalPath checks that the log records the path each version was
// written at: pre-move entries stay under the old path, the move entry lands
// under the new one.
func TestMoveHistoricalPath(t *testing.T) {
	a, b := uniqPath("mhp-a"), uniqPath("mhp-b")
	mkParam(t, a, "text/plain", "hi", "create")
	editParam(t, a, "text/plain", "hi2", "edit")
	moveParam(t, a, b, "move")

	newLog := requireLog(t, b, 1)
	if want := "Moved from " + a + ". move"; newLog[0].Comment != want {
		t.Errorf("move entry comment = %q, want %q", newLog[0].Comment, want)
	}
	oldLog := requireLog(t, a, 2)
	if oldLog[0].Comment != "edit" || oldLog[1].Comment != "create" {
		t.Errorf("old path log = %+v, want pre-move edit+create", oldLog)
	}
}

// TestMoveOntoDeletedPath checks moving a node onto a soft-deleted path: the
// displaced tombstone is renamed aside (<path>#<id>) but the log keeps its
// real path, so the path history shows the former occupant alongside the new
// one.
func TestMoveOntoDeletedPath(t *testing.T) {
	victim, mover := uniqPath("victim"), uniqPath("mover")
	mkParam(t, victim, "text/plain", "v", "create victim")
	deleteParam(t, victim, "delete victim")
	mkParam(t, mover, "text/plain", "m", "create mover")
	moveParam(t, mover, victim, "takeover")

	log := requireLog(t, victim, 3)
	for i, want := range []struct {
		comment string
		deleted bool
	}{
		{"Moved from " + mover + ". takeover", false},
		{"delete victim", true},
		{"create victim", false},
	} {
		if log[i].Comment != want.comment || log[i].Deleted != want.deleted {
			t.Errorf("entry %d = %+v, want comment %q deleted %v", i, log[i], want.comment, want.deleted)
		}
	}
	requireLog(t, mover, 1)

	tombstones := dbInt(t, "SELECT COUNT(*) FROM my_config_tree WHERE Path LIKE '"+victim+"#%' AND Deleted")
	if tombstones != 1 {
		t.Errorf("renamed-aside tombstones at %s#...: got %d, want 1", victim, tombstones)
	}
}

// TestSubtreeMoveLogging checks that moving a subtree logs every live
// descendant at its new path, not just the root.
func TestSubtreeMoveLogging(t *testing.T) {
	s, s2 := uniqPath("st"), uniqPath("st2")
	mkParam(t, s, "application/x-null", "", "mk root")
	mkParam(t, s+"/sub", "application/x-null", "", "mk sub")
	mkParam(t, s+"/sub/leaf", "text/plain", "hi", "mk leaf")
	moveParam(t, s, s2, "relocate")

	moveComment := "Moved from " + s + ". relocate"
	for _, path := range []string{s2, s2 + "/sub", s2 + "/sub/leaf"} {
		log := requireLog(t, path, 1)
		if !strings.HasPrefix(log[0].Comment, moveComment) {
			t.Errorf("log of %s: comment = %q, want %q", path, log[0].Comment, moveComment)
		}
	}
	old := requireLog(t, s+"/sub", 1)
	if old[0].Comment != "mk sub" {
		t.Errorf("old descendant path log = %+v, want its pre-move create", old)
	}
}
