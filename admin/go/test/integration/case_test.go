//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ugorji/go/codec"
)

// tryFetchClientConfigValues fetches /client/config as the given service
// account (hostless: the compose network falls into the example ephemeral-ip
// range, so hostname-based conditions are out of the picture by
// construction) and returns the delivered values by path with the HTTP
// status. A freshly created account yields 401 until the resolver loads the
// tree that carries it — the same reload the convergence polls wait for —
// so callers polling for convergence must tolerate it.
func tryFetchClientConfigValues(t *testing.T, user, pass string) (map[string]string, int) {
	t.Helper()
	req, err := http.NewRequest("GET", baseURL+"/client/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("X-OnlineConf-Client-Version", "TEST")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET /client/config as %s: %v", user, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var config struct {
		Nodes []struct {
			Path        string
			ContentType string
			Value       []uint8
		} `json:"nodes"`
	}
	if err := codec.NewDecoder(resp.Body, &codec.CborHandle{}).Decode(&config); err != nil {
		t.Fatalf("decode /client/config as %s: %v", user, err)
	}
	values := make(map[string]string, len(config.Nodes))
	for _, n := range config.Nodes {
		values[n.Path] = string(n.Value)
	}
	return values, resp.StatusCode
}

func fetchClientConfigValues(t *testing.T, user, pass string) map[string]string {
	t.Helper()
	values, status := tryFetchClientConfigValues(t, user, pass)
	if status != http.StatusOK {
		t.Fatalf("GET /client/config as %s: status %d", user, status)
	}
	return values
}

// TestCaseServiceSlashNames pins down case resolution for slash-separated
// service names: a group defined by the glob `service:*/rc` must contain
// every account whose name ends in /rc (group globs are dot-separated, so a
// `*` crosses slashes freely), an exact service branch must match only the
// full slashed name, and the slash-prefix ladder must let a `service: <base>`
// branch match every account under that base.
func TestCaseServiceSlashNames(t *testing.T) {
	pid := fmt.Sprint(os.Getpid())
	base := "itslash" + pid
	group := "it-rcg-" + pid

	pwd := func(name string) string { return name + "-secret" }
	account := func(name string) {
		t.Helper()
		hash := sha256.Sum256([]byte(pwd(name)))
		mkParam(t, "/onlineconf/service/"+name, "text/plain", hex.EncodeToString(hash[:]), "slash service account")
	}
	mkParam(t, "/onlineconf/service/"+base, "application/x-null", "", "container")
	mkParam(t, "/onlineconf/service/"+base+"/foo", "application/x-null", "", "container")
	mkParam(t, "/onlineconf/service/"+base+"/bar", "application/x-null", "", "container")
	account(base + "/foo/rc")
	account(base + "/bar/rc")
	account(base + "/other")

	// group membership by service name: the `*` must cross the slash inside
	// "<base>/foo"
	mkParam(t, "/onlineconf/group/"+group, "text/plain", "service:*/rc", "slash service group")

	module := "/onlineconf/module/itcase" + pid
	mkParam(t, module, "application/x-null", "", "test module")

	caseValue := func(innerCondition, innerValue string) string {
		inner, err := json.Marshal([]map[string]string{
			{"service": innerCondition, "mime": "text/plain", "value": innerValue},
			{"mime": "text/plain", "value": "inner-default"},
		})
		if err != nil {
			t.Fatal(err)
		}
		outer, err := json.Marshal([]map[string]string{
			{"group": group, "mime": "application/x-case", "value": string(inner)},
			{"mime": "text/plain", "value": "outer-default"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(outer)
	}
	mkParam(t, module+"/exact", "application/x-case", caseValue(base+"/foo/rc", "foo-hit"), "exact slashed service")
	mkParam(t, module+"/prefix", "application/x-case", caseValue(base, "prefix-hit"), "service prefix")
	// written last: once it is delivered, the served tree includes everything
	// created above
	mkParam(t, module+"/sentinel", "text/plain", "sentinel", "convergence anchor")

	defer func() {
		deleteParam(t, module+"/sentinel", "cleanup")
		deleteParam(t, module+"/prefix", "cleanup")
		deleteParam(t, module+"/exact", "cleanup")
		deleteParam(t, module, "cleanup")
		deleteParam(t, "/onlineconf/group/"+group, "cleanup")
		deleteParam(t, "/onlineconf/service/"+base+"/other", "cleanup")
		deleteParam(t, "/onlineconf/service/"+base+"/bar/rc", "cleanup")
		deleteParam(t, "/onlineconf/service/"+base+"/bar", "cleanup")
		deleteParam(t, "/onlineconf/service/"+base+"/foo/rc", "cleanup")
		deleteParam(t, "/onlineconf/service/"+base+"/foo", "cleanup")
		deleteParam(t, "/onlineconf/service/"+base, "cleanup")
	}()

	deadline := time.Now().Add(90 * time.Second)
	for {
		values, status := tryFetchClientConfigValues(t, base+"/foo/rc", pwd(base+"/foo/rc"))
		if status == http.StatusOK && values[module+"/sentinel"] == "sentinel" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sentinel parameter never reached /client/config (last status %d)", status)
		}
		time.Sleep(2 * time.Second)
	}

	expect := map[string]struct{ exact, prefix string }{
		base + "/foo/rc": {"foo-hit", "prefix-hit"},          // group via */rc, inner exact and prefix both match
		base + "/bar/rc": {"inner-default", "prefix-hit"},    // group matches, exact does not, prefix does
		base + "/other":  {"outer-default", "outer-default"}, // none of its prefixes ends in /rc
	}
	for _, name := range []string{base + "/foo/rc", base + "/bar/rc", base + "/other"} {
		values := fetchClientConfigValues(t, name, pwd(name))
		want := expect[name]
		if got := values[module+"/exact"]; got != want.exact {
			t.Errorf("%s: exact-branch param = %q, want %q", name, got, want.exact)
		}
		if got := values[module+"/prefix"]; got != want.prefix {
			t.Errorf("%s: prefix-branch param = %q, want %q", name, got, want.prefix)
		}
	}
}
