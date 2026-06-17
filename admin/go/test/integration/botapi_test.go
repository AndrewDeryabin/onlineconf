//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type botNotification struct {
	ID     int    `json:"id"`
	Path   string `json:"path"`
	Action string `json:"action"`
}

type botNotificationsResponse struct {
	Notifications []botNotification `json:"notifications"`
	LastID        int               `json:"lastID"`
}

// botGet queries the botapi notification feed with the bot's own basic-auth
// credentials (botapi has its own authentication, no CSRF guard).
func botGet(t *testing.T, bot, password string, lastID, limit int) (int, *botNotificationsResponse) {
	t.Helper()
	url := fmt.Sprintf("%s/botapi/notification/?lastID=%d&limit=%d", baseURL, lastID, limit)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(bot, password)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var r botNotificationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp.StatusCode, &r
}

// TestBotapiSubtreeMoveSilent checks that the botapi notification feed (used
// by onlineconf-bot, the non-legacy delivery path) serves exactly one
// notification for a subtree move — the root — while the relocated
// descendants' log entries are marked Silent and skipped.
func TestBotapiSubtreeMoveSilent(t *testing.T) {
	// botapi credentials live in the config tree itself and are picked up by
	// the resolver-backed synchronizer within ~5s
	bot := fmt.Sprintf("it-bot-%d", os.Getpid())
	const password = "it-bot-password"
	hash := sha256.Sum256([]byte(password))
	ensureParam(t, "/onlineconf/botapi", "application/x-null", "", "botapi root")
	ensureParam(t, "/onlineconf/botapi/bot", "application/x-null", "", "botapi accounts")
	mkParam(t, "/onlineconf/botapi/bot/"+bot, "text/plain", hex.EncodeToString(hash[:]), "bot account")
	mkParam(t, "/onlineconf/botapi/bot/"+bot+"/scopes", "application/x-list", "notifications", "bot scopes")

	var baseline *botNotificationsResponse
	deadline := time.Now().Add(60 * time.Second)
	for {
		var status int
		if status, baseline = botGet(t, bot, password, 0, 0); status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("botapi credentials did not become active, last status %d", status)
		}
		time.Sleep(time.Second)
	}

	s, s2 := uniqPath("bsm"), uniqPath("bsm2")
	mkParam(t, s, "application/x-null", "", "mk root")
	mkParam(t, s+"/sub", "application/x-null", "", "mk sub")
	mkParam(t, s+"/sub/leaf", "text/plain", "hi", "mk leaf")
	setNotification(t, s, "with-value")
	moveParam(t, s, s2, "relocate")

	status, feed := botGet(t, bot, password, baseline.LastID, 100)
	if status != http.StatusOK {
		t.Fatalf("botapi feed: status %d", status)
	}
	var moves int
	for _, n := range feed.Notifications {
		switch {
		case n.Path == s2:
			moves++
		case strings.HasPrefix(n.Path, s2+"/"):
			t.Errorf("descendant notification leaked into the feed: %+v", n)
		}
	}
	if moves != 1 {
		t.Errorf("feed contains %d notifications for the moved root %s, want exactly 1", moves, s2)
	}

	// the descendant entries exist in the log — they are just marked Silent
	prefix := s2 + "/%"
	if got := dbInt(t, "SELECT COUNT(*) FROM my_config_tree_log WHERE Path LIKE '"+prefix+"'"); got != 2 {
		t.Errorf("descendant log entries under %s: got %d, want 2", s2, got)
	}
	if got := dbInt(t, "SELECT COUNT(*) FROM my_config_tree_log WHERE Path LIKE '"+prefix+"' AND Silent"); got != 2 {
		t.Errorf("Silent descendant log entries under %s: got %d, want 2", s2, got)
	}
	if got := dbInt(t, "SELECT Silent FROM my_config_tree_log WHERE Path = '"+s2+"' ORDER BY ID DESC LIMIT 1"); got != 0 {
		t.Errorf("the root move entry is marked Silent, want deliverable")
	}
}
