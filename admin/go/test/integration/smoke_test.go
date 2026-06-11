//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestSmoke checks that the stack is up, auth and the CSRF guard behave, and
// the core read endpoints serve.
func TestSmoke(t *testing.T) {
	for _, ep := range []string{"/whoami", "/config/", "/log/", "/global-log"} {
		if status, body := adminReq(t, "GET", ep, nil); status != http.StatusOK {
			t.Errorf("GET %s: status %d, body %s", ep, status, body)
		}
	}

	// a request without X-Requested-With must be rejected by the CSRF guard
	req, err := http.NewRequest("GET", baseURL+"/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("admin", "admin")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /whoami without X-Requested-With: status %d, want 403", resp.StatusCode)
	}

	var who struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(adminReqOK(t, "GET", "/whoami", nil), &who); err != nil {
		t.Fatal(err)
	}
	if who.Username != "admin" {
		t.Errorf("/whoami username = %q, want admin", who.Username)
	}
}
