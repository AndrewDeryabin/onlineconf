//go:build integration

package integration

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestNotifyOnCommit checks that legacy (notifyDB) change notifications are
// delivered only on commit: a rejected write emits none, and a subtree move
// emits exactly one — for the root, with descendants logged but not notified.
func TestNotifyOnCommit(t *testing.T) {
	n, n2 := uniqPath("ntf"), uniqPath("ntf2")
	mkParam(t, n, "application/x-null", "", "mk root")
	mkParam(t, n+"/child", "text/plain", "hi", "mk child")
	setNotification(t, n, "with-value")

	const countQuery = "SELECT COUNT(*) FROM my_change_notification"
	before := dbInt(t, countQuery)

	status, body := adminReq(t, "POST", "/config"+n, url.Values{
		"version": {"999999"},
		"path":    {n2},
		"comment": {"bad"},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "VersionNotMatch") {
		t.Fatalf("stale-version move: status %d, body %s, want 400 VersionNotMatch", status, body)
	}
	if got := dbInt(t, countQuery); got != before {
		t.Errorf("rejected move changed notification count: %d -> %d (phantom notification)", before, got)
	}

	moveParam(t, n, n2, "relocate")
	if got := dbInt(t, countQuery); got != before+1 {
		t.Errorf("successful subtree move: notification count %d -> %d, want exactly +1 (root only)", before, got)
	}
	last := dbQuery(t, "SELECT Message FROM my_change_notification ORDER BY ID DESC LIMIT 1")
	if len(last) != 1 || !strings.Contains(last[0][0], n2) {
		t.Errorf("delivered notification %v does not mention the moved root %s", last, n2)
	}
}
