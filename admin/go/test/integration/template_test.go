//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ugorji/go/codec"
)

// --- helpers ----------------------------------------------------------------

type paramInfo struct {
	Mime     string `json:"mime"`
	Template bool   `json:"template"`
	Data     string `json:"data"`
	Version  int    `json:"version"`
}

func getParamInfo(t *testing.T, path string) paramInfo {
	t.Helper()
	body := adminReqOK(t, "GET", "/config"+path, nil)
	var p paramInfo
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("parse %s: %v in %s", path, err, body)
	}
	return p
}

// mkTemplateParam creates a parameter with the template flag set.
func mkTemplateParam(t *testing.T, path, mime, data, comment string) {
	t.Helper()
	adminReqOK(t, "POST", "/config"+path, url.Values{
		"mime": {mime}, "template": {"1"}, "data": {data}, "comment": {comment},
	})
}

// --- tests ------------------------------------------------------------------

// TestTemplateFlagCRUD exercises the flag through create/edit/clear: the flag
// is stored, reported, preserved by writes that do not mention it (tri-state)
// and cleared by an explicit template=0.
func TestTemplateFlagCRUD(t *testing.T) {
	path := uniqPath("tf-crud")
	mkTemplateParam(t, path, "text/plain", "v ${x}", "create flagged")
	defer deleteParam(t, path, "cleanup")

	if p := getParamInfo(t, path); !p.Template || p.Mime != "text/plain" {
		t.Fatalf("created param: mime %s template %v, want text/plain true", p.Mime, p.Template)
	}

	// a write without the field must not strip the flag
	editParam(t, path, "text/plain", "v2 ${x}", "edit without the field")
	if p := getParamInfo(t, path); !p.Template {
		t.Fatal("the flag was stripped by a write that did not mention it")
	}

	// an explicit template=0 clears it
	adminReqOK(t, "POST", "/config"+path, url.Values{
		"version":  {strconv.Itoa(paramVersion(t, path))},
		"mime":     {"text/plain"},
		"template": {"0"},
		"data":     {"v3"},
		"comment":  {"clear the flag"},
	})
	if p := getParamInfo(t, path); p.Template {
		t.Fatal("template=0 did not clear the flag")
	}

	// the log carries the flag per version
	body := adminReqOK(t, "GET", "/log"+path, nil)
	var entries []struct {
		Version  int  `json:"version"`
		Template bool `json:"template"`
	}
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatalf("parse log: %v in %s", err, body)
	}
	byVersion := map[int]bool{}
	for _, e := range entries {
		byVersion[e.Version] = e.Template
	}
	if !byVersion[1] || !byVersion[2] || byVersion[3] {
		t.Errorf("log template flags by version: %v, want 1:true 2:true 3:false", byVersion)
	}

	// the flag is rejected on types that cannot carry it
	status, body2 := adminReq(t, "POST", "/config"+uniqPath("tf-crud-null"), url.Values{
		"mime": {"application/x-null"}, "template": {"1"}, "comment": {"flag on null"},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body2), "TemplateFlagNotAllowed") {
		t.Errorf("flag on null: status %d body %s, want 400 TemplateFlagNotAllowed", status, body2)
	}
}

// TestTemplateFlagGated checks the opt-out switch: a fresh stack is enabled by
// default (absent parameter), creating /onlineconf/template-flag = 0 refuses
// template-carrying writes and hides the feature from /ui-config, and lifting
// the opt-out restores it. Also asserts /ui-config advertises the predefined
// template variables.
func TestTemplateFlagGated(t *testing.T) {
	const flagParam = "/onlineconf/template-flag"

	uiConfig := func() (bool, []string) {
		body := adminReqOK(t, "GET", "/ui-config", nil)
		var cfg struct {
			TemplateFlag bool     `json:"template_flag"`
			TemplateVars []string `json:"template_vars"`
		}
		if err := json.Unmarshal(body, &cfg); err != nil {
			t.Fatalf("parse ui-config: %v in %s", err, body)
		}
		return cfg.TemplateFlag, cfg.TemplateVars
	}

	if enabled, vars := uiConfig(); !enabled {
		t.Fatal("a fresh stack must have the template flag enabled (absent parameter = on)")
	} else {
		want := []string{"hostname", "hostname -s", "short_hostname", "hostname -i", "ip"}
		if fmt.Sprint(vars) != fmt.Sprint(want) {
			t.Errorf("ui-config template_vars = %v, want %v", vars, want)
		}
	}

	// opt out and verify the gate
	ensureParam(t, flagParam, "text/plain", "0", "opt out")
	defer deleteParam(t, flagParam, "lift the opt-out")

	if enabled, _ := uiConfig(); enabled {
		t.Error("ui-config still reports the feature on while opted out")
	}
	status, body := adminReq(t, "POST", "/config"+uniqPath("tf-gated"), url.Values{
		"mime": {"text/plain"}, "template": {"1"}, "data": {"v"}, "comment": {"gated"},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "TemplateFlagDisabled") {
		t.Fatalf("write with the field while opted out: status %d body %s, want 400 TemplateFlagDisabled", status, body)
	}
	// even template=0 is refused: the client must not run ahead of the operator
	status, body = adminReq(t, "POST", "/config"+uniqPath("tf-gated0"), url.Values{
		"mime": {"text/plain"}, "template": {"0"}, "data": {"v"}, "comment": {"gated"},
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "TemplateFlagDisabled") {
		t.Fatalf("write with template=0 while opted out: status %d body %s, want 400 TemplateFlagDisabled", status, body)
	}

	// lift the opt-out by value
	editParam(t, flagParam, "text/plain", "1", "lift the opt-out")
	if enabled, _ := uiConfig(); !enabled {
		t.Error("ui-config still reports the feature off after lifting the opt-out")
	}
}

// TestTypedTemplateDelivery is the headline capability end to end: a flagged
// application/json parameter is delivered to updaters with its declared type
// and substituted content, while one with a non-server variable is delivered
// as application/x-template — the wire marker for "the updater finishes the
// job".
func TestTypedTemplateDelivery(t *testing.T) {
	pid := fmt.Sprint(os.Getpid())
	svc := "it-svc-" + pid
	password := "it-secret"
	hash := sha256.Sum256([]byte(password))
	ensureParam(t, "/onlineconf/service/"+svc, "text/plain", hex.EncodeToString(hash[:]), "test service account")
	defer deleteParam(t, "/onlineconf/service/"+svc, "cleanup")

	vars := uniqPath("tdel-vars")
	mkParam(t, vars, "application/x-null", "", "container")
	mkParam(t, vars+"/host", "text/plain", "h1", "host")
	mkParam(t, vars+"/port", "text/plain", "8080", "port")

	module := "/onlineconf/module/itmod" + pid
	mkParam(t, module, "application/x-null", "", "test module")
	mkTemplateParam(t, module+"/typed", "application/json",
		`{"host": "${`+vars+`/host}", "port": ${`+vars+`/port}}`, "typed template")
	mkTemplateParam(t, module+"/dirty", "text/plain", "x ${updater_local_var}", "dirty template")
	defer func() {
		deleteParam(t, module+"/dirty", "cleanup")
		deleteParam(t, module+"/typed", "cleanup")
		deleteParam(t, module, "cleanup")
		deleteParam(t, vars+"/port", "cleanup")
		deleteParam(t, vars+"/host", "cleanup")
		deleteParam(t, vars, "cleanup")
	}()

	// The resolver reloads its tree every 5s, but on a cold stack its first
	// successful load can lag well behind the admin API becoming ready
	// (waitReady exercises only the auth path) — poll generously; the test
	// asserts convergence, not latency.
	type node struct {
		Path        string
		ContentType string
		Value       []uint8
	}
	var nodes map[string]node
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest("GET", baseURL+"/client/config", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(svc, password)
		req.Header.Set("X-OnlineConf-Client-Version", "TEST")
		req.Header.Set("X-OnlineConf-Client-Host", "it-host-"+pid)
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == http.StatusOK {
			var config struct {
				Nodes []node `json:"nodes"`
			}
			err = codec.NewDecoder(resp.Body, &codec.CborHandle{}).Decode(&config)
			resp.Body.Close()
			if err != nil {
				t.Fatalf("decode /client/config: %v", err)
			}
			nodes = map[string]node{}
			for _, n := range config.Nodes {
				nodes[n.Path] = n
			}
			if _, ok := nodes[module+"/typed"]; ok {
				break
			}
		} else {
			resp.Body.Close()
		}
		time.Sleep(2 * time.Second)
	}

	typed, ok := nodes[module+"/typed"]
	if !ok {
		t.Fatalf("typed parameter never appeared in /client/config; got %d nodes", len(nodes))
	}
	if typed.ContentType != "application/json" {
		t.Errorf("typed param delivered as %s, want application/json", typed.ContentType)
	}
	if want := `{"host": "h1", "port": 8080}`; string(typed.Value) != want {
		t.Errorf("typed param value = %s, want %s", typed.Value, want)
	}

	dirty, ok := nodes[module+"/dirty"]
	if !ok {
		t.Fatal("dirty parameter missing from /client/config")
	}
	if dirty.ContentType != "application/x-template" {
		t.Errorf("dirty param delivered as %s, want application/x-template (the client-side marker)", dirty.ContentType)
	}
	if want := "x ${updater_local_var}"; string(dirty.Value) != want {
		t.Errorf("dirty param value = %s, want %s", dirty.Value, want)
	}
}

// TestFlaggedReferrerBlocksDelete checks that flagged parameters protect the
// targets of their ${/path} variables exactly like legacy templates: at the
// node level, from a case branch, and through a symlink hop.
func TestFlaggedReferrerBlocksDelete(t *testing.T) {
	base := uniqPath("tf-ref")
	mkParam(t, base, "application/x-null", "", "container")
	mkParam(t, base+"/target", "text/plain", "v", "target")
	mkParam(t, base+"/link", "application/x-symlink", base, "hop")

	flagged := uniqPath("tf-ref-direct")
	mkTemplateParam(t, flagged, "application/json", `{"t": "${`+base+`/link/target}"}`, "flagged referrer through a hop")

	caseRef := uniqPath("tf-ref-case")
	mkParam(t, caseRef, "application/x-case",
		`[{"group":"g","mime":"text/plain","template":true,"value":"${`+base+`/target}"},{"mime":"text/plain","value":"d"}]`,
		"flagged case branch referrer")

	// only the direct referrer resolves through the hop (the error code
	// names the kind of the referrer, so a template referrer reports
	// DeletedTemplate even for a symlink being deleted)
	requireBlockedDelete(t, base+"/link", "DeletedTemplate", flagged)
	// both referrers hold edges to the terminal; the symlinked bucket wins
	// the error code, and a case referrer counts as "symlinked" whatever
	// its branches hold — the buckets follow the referrer node's kind, as
	// they always did for legacy template branches
	requireBlockedDelete(t, base+"/target", "DeletedSymlink", caseRef)

	deleteParam(t, caseRef, "drop the case referrer")
	requireBlockedDelete(t, base+"/target", "DeletedTemplate", flagged)

	deleteParam(t, flagged, "drop the direct referrer")
	deleteParam(t, base+"/link", "cleanup")
	deleteParam(t, base+"/target", "cleanup")
	deleteParam(t, base, "cleanup")
}

// TestFlaggedReferrerHealed checks the dangling-point heal for flagged
// parameters: a ${/path} written before its target exists starts protecting
// the target the moment it is created.
func TestFlaggedReferrerHealed(t *testing.T) {
	future := uniqPath("tf-heal") + "/future"
	referrer := uniqPath("tf-heal-ref")
	mkTemplateParam(t, referrer, "text/plain", "${"+future+"}", "dangling flagged referrer")

	mkParam(t, uniqPath("tf-heal"), "application/x-null", "", "container")
	mkParam(t, future, "text/plain", "arrived", "the future target")

	requireBlockedDelete(t, future, "DeletedTemplate", referrer)

	deleteParam(t, referrer, "cleanup")
	deleteParam(t, future, "cleanup")
	deleteParam(t, uniqPath("tf-heal"), "cleanup")
}

// TestRebuildSeedsFlagged checks the rebuild seeding arm: after the dep rows
// of a flagged referrer are lost, a full rebuild (triggered by raising the
// depth parameter) re-discovers it.
func TestRebuildSeedsFlagged(t *testing.T) {
	const depthParam = "/onlineconf/deleted-key-symlinks-check-depth"

	target := uniqPath("tf-seed-tgt")
	referrer := uniqPath("tf-seed-ref")
	mkParam(t, target, "text/plain", "v", "target")
	mkTemplateParam(t, referrer, "text/plain", "${"+target+"}", "flagged referrer")

	// simulate lost edges, then force a rebuild by raising the depth
	dbQuery(t, "DELETE FROM my_config_tree_dep WHERE ReferrerID = (SELECT ID FROM my_config_tree WHERE Path = '"+referrer+"')")
	if status, _ := tryDelete(t, target); status != http.StatusOK {
		t.Fatalf("sanity: without its dep rows the referrer cannot block (status %d)", status)
	}
	mkParam(t, target, "text/plain", "v", "recreate the target") // tryDelete above removed it
	// recreating the target heals the referrer; wipe the rows again to
	// isolate the rebuild path
	dbQuery(t, "DELETE FROM my_config_tree_dep WHERE ReferrerID = (SELECT ID FROM my_config_tree WHERE Path = '"+referrer+"')")

	newDepth := dbInt(t, "SELECT Depth FROM my_config_tree_dep_meta LIMIT 1") + 1
	if status, _ := adminReq(t, "GET", "/config"+depthParam, nil); status == http.StatusOK {
		editParam(t, depthParam, "text/plain", strconv.Itoa(newDepth), "raise to force a rebuild")
	} else {
		mkParam(t, depthParam, "text/plain", strconv.Itoa(newDepth), "raise to force a rebuild")
	}
	defer deleteParam(t, depthParam, "restore default depth")

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if dbInt(t, "SELECT Depth FROM my_config_tree_dep_meta LIMIT 1") == newDepth {
			break
		}
		time.Sleep(time.Second)
	}

	requireBlockedDelete(t, target, "DeletedTemplate", referrer)

	deleteParam(t, referrer, "cleanup")
	deleteParam(t, target, "cleanup")
}

// TestConvertTemplateTypes runs the one-shot conversion against seeded legacy
// templates (a node value and a nested case branch) and asserts the rewrite,
// the Silent log rows, the dependency-edge invariance and idempotency.
func TestConvertTemplateTypes(t *testing.T) {
	target := uniqPath("tconv-tgt")
	legacy := uniqPath("tconv-legacy")
	nested := uniqPath("tconv-case")
	mkParam(t, target, "text/plain", "v", "target")
	mkParam(t, legacy, "application/x-template", "${"+target+"}:${port}", "legacy template")
	mkParam(t, nested, "application/x-case",
		`[{"datacenter":"dc","mime":"application/x-case","value":"[{\"group\":\"g\",\"mime\":\"application/x-template\",\"value\":\"${`+target+`}\"}]"},{"mime":"text/plain","value":"d"}]`,
		"nested legacy branch")

	edges := func(path string) int {
		return dbInt(t, "SELECT COUNT(*) FROM my_config_tree_dep WHERE ReferrerID = (SELECT ID FROM my_config_tree WHERE Path = '"+path+"')")
	}
	legacyEdges, nestedEdges := edges(legacy), edges(nested)
	legacyVersion := paramVersion(t, legacy)

	convert := func() string {
		out, err := composeStdout("exec", "-T", "onlineconf-admin", "onlineconf-admin", "-convert-template-types")
		if err != nil {
			t.Fatalf("conversion failed: %v\n%s", err, out)
		}
		return out
	}

	// while opted out the tool refuses to run — converting rows old binaries
	// may still read would corrupt what they serve
	ensureParam(t, "/onlineconf/template-flag", "text/plain", "0", "opt out")
	if _, err := composeStdout("exec", "-T", "onlineconf-admin", "onlineconf-admin", "-convert-template-types"); err == nil {
		t.Fatal("the conversion ran despite the opt-out")
	}
	deleteParam(t, "/onlineconf/template-flag", "lift the opt-out")

	convert()

	if p := getParamInfo(t, legacy); p.Mime != "text/plain" || !p.Template || p.Data != "${"+target+"}:${port}" {
		t.Errorf("converted node: mime %s template %v data %s", p.Mime, p.Template, p.Data)
	}
	if p := getParamInfo(t, nested); strings.Contains(p.Data, "application/x-template") || !strings.Contains(p.Data, `\"template\":true`) {
		t.Errorf("converted case value: %s", p.Data)
	}
	if got := edges(legacy); got != legacyEdges {
		t.Errorf("conversion changed the node referrer's edges: %d -> %d", legacyEdges, got)
	}
	if got := edges(nested); got != nestedEdges {
		t.Errorf("conversion changed the case referrer's edges: %d -> %d", nestedEdges, got)
	}
	if n := dbInt(t, "SELECT COUNT(*) FROM my_config_tree WHERE NOT Deleted AND ContentType = 'application/x-template'"); n != 0 {
		t.Errorf("%d live legacy template rows remain", n)
	}
	silent := dbInt(t, "SELECT COUNT(*) FROM my_config_tree_log l JOIN my_config_tree p ON p.ID = l.NodeID"+
		" WHERE p.Path IN ('"+legacy+"', '"+nested+"') AND l.Silent AND l.Comment LIKE 'template content type%'")
	if silent != 2 {
		t.Errorf("expected 2 silent conversion log rows, found %d", silent)
	}
	// the protection survives the conversion; with both referrers alive the
	// symlinked bucket (the case) wins the error code and names the case
	requireBlockedDelete(t, target, "DeletedSymlink", nested)

	// idempotent: a re-run converts nothing
	convert()
	if v := paramVersion(t, legacy); v != legacyVersion+1 {
		t.Errorf("version after two conversion runs: %d, want %d (exactly one bump)", v, legacyVersion+1)
	}

	deleteParam(t, nested, "cleanup")
	deleteParam(t, legacy, "cleanup")
	deleteParam(t, target, "cleanup")
}

// TestLogRestorePreservesFlag replays what the UI rollback does: posting a
// historical version's mime/data/template restores the flag, and a legacy
// application/x-template version still restores while its write support
// lasts.
func TestLogRestorePreservesFlag(t *testing.T) {
	path := uniqPath("tf-restore")
	mkTemplateParam(t, path, "text/plain", "v1 ${x}", "flagged v1")
	adminReqOK(t, "POST", "/config"+path, url.Values{
		"version":  {strconv.Itoa(paramVersion(t, path))},
		"mime":     {"text/plain"},
		"template": {"0"},
		"data":     {"v2"},
		"comment":  {"unflagged v2"},
	})

	// restore v1 the way RollbackDialog does
	adminReqOK(t, "POST", "/config"+path, url.Values{
		"version":  {strconv.Itoa(paramVersion(t, path))},
		"mime":     {"text/plain"},
		"template": {"1"},
		"data":     {"v1 ${x}"},
		"comment":  {"restore v1"},
	})
	if p := getParamInfo(t, path); !p.Template || p.Data != "v1 ${x}" {
		t.Errorf("restored param: template %v data %s, want true 'v1 ${x}'", p.Template, p.Data)
	}
	deleteParam(t, path, "cleanup")

	// a legacy-typed version still restores (write support lasts until the
	// sunset release)
	legacy := uniqPath("tf-restore-legacy")
	mkParam(t, legacy, "application/x-template", "${x}", "legacy v1")
	editParam(t, legacy, "text/plain", "plain v2", "v2")
	adminReqOK(t, "POST", "/config"+legacy, url.Values{
		"version": {strconv.Itoa(paramVersion(t, legacy))},
		"mime":    {"application/x-template"},
		"data":    {"${x}"},
		"comment": {"restore legacy v1"},
	})
	if p := getParamInfo(t, legacy); p.Mime != "application/x-template" || p.Template {
		t.Errorf("restored legacy param: mime %s template %v", p.Mime, p.Template)
	}
	deleteParam(t, legacy, "cleanup")
}

// TestResurrectClearsFlag checks that re-creating a soft-deleted path does not
// leak the zombie's template flag into the new parameter.
func TestResurrectClearsFlag(t *testing.T) {
	path := uniqPath("tf-resurrect")
	mkTemplateParam(t, path, "text/plain", "flagged life", "v1")
	deleteParam(t, path, "die")

	mkParam(t, path, "text/plain", "plain life", "resurrect without the flag")
	if p := getParamInfo(t, path); p.Template {
		t.Error("the resurrected parameter inherited the zombie's template flag")
	}
	if n := dbInt(t, "SELECT Templated FROM my_config_tree WHERE Path = '"+path+"'"); n != 0 {
		t.Errorf("Templated column = %d after resurrection, want 0", n)
	}
	deleteParam(t, path, "cleanup")
}
