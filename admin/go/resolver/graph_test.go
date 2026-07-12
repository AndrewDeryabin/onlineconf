//go:build !integration

package resolver

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// stubCaseResolver picks the first branch of every case and serves template
// variables from a fixed map — enough context to drive graph.resolve without
// a server.
type stubCaseResolver struct {
	vars map[string]string
}

func (r stubCaseResolver) resolveCase(_ context.Context, param *Param) *Case {
	var cases []Case
	if err := json.Unmarshal([]byte(param.Value.String), &cases); err != nil || len(cases) == 0 {
		return nil
	}
	return &cases[0]
}

func (r stubCaseResolver) getTemplateVar(name string) (string, bool) {
	v, ok := r.vars[name]
	return v, ok
}

func testParam(path, contentType, value string, templated bool) *Param {
	return &Param{
		Path:        path,
		Name:        path[strings.LastIndex(path, "/")+1:],
		ContentType: contentType,
		Templated:   templated,
		Value:       NullString{NullString: sql.NullString{String: value, Valid: contentType != "application/x-null"}},
		Children:    map[string]**Param{},
	}
}

// testGraph builds a resolution graph from (path, param) pairs; intermediate
// nodes must be listed before their children.
func testGraph(resolver caseResolver, params ...*Param) *graph {
	root := testParam("/", "application/x-null", "", false)
	byPath := map[string]*Param{"/": root}
	for _, p := range params {
		parentPath := p.Path[:strings.LastIndex(p.Path, "/")]
		if parentPath == "" {
			parentPath = "/"
		}
		parent := byPath[parentPath]
		if parent == nil {
			panic("no parent for " + p.Path)
		}
		node := p
		parent.Children[node.Name] = &node
		byPath[p.Path] = node
	}
	return &graph{root: root, caseResolver: resolver}
}

func TestFlaggedParamKeepsDeclaredMime(t *testing.T) {
	g := testGraph(stubCaseResolver{vars: map[string]string{"port": "8080"}},
		testParam("/vars", "application/x-null", "", false),
		testParam("/vars/host", "text/plain", "h1", false),
		testParam("/cfg", "application/json", `{"host": "${/vars/host}", "port": ${port}}`, true),
	)
	p := g.get(context.Background(), "/cfg")
	if p == nil {
		t.Fatal("param not found")
	}
	if p.ContentType != "application/json" {
		t.Errorf("mime = %s, want application/json (the declared type survives expansion)", p.ContentType)
	}
	if want := `{"host": "h1", "port": 8080}`; p.Value.String != want {
		t.Errorf("value = %s, want %s", p.Value.String, want)
	}
	if p.Templated {
		t.Error("Templated must be cleared after evaluation")
	}
}

func TestFlaggedDirtyDeliveredAsLegacyTemplate(t *testing.T) {
	g := testGraph(stubCaseResolver{},
		testParam("/cfg", "text/plain", "x ${updater_local_var}", true),
	)
	p := g.get(context.Background(), "/cfg")
	if p.ContentType != "application/x-template" {
		t.Errorf("mime = %s, want application/x-template (the wire marker for leftover placeholders)", p.ContentType)
	}
	if want := "x ${updater_local_var}"; p.Value.String != want {
		t.Errorf("value = %s, want %s", p.Value.String, want)
	}
}

func TestLegacyTemplateStillExpandsToText(t *testing.T) {
	g := testGraph(stubCaseResolver{},
		testParam("/vars", "application/x-null", "", false),
		testParam("/vars/host", "text/plain", "h1", false),
		testParam("/cfg", "application/x-template", "${/vars/host}", false),
	)
	p := g.get(context.Background(), "/cfg")
	if p.ContentType != "text/plain" {
		t.Errorf("mime = %s, want text/plain", p.ContentType)
	}
	if p.Value.String != "h1" {
		t.Errorf("value = %s, want h1", p.Value.String)
	}
}

func TestDirtyTargetMakesReferrerDirty(t *testing.T) {
	g := testGraph(stubCaseResolver{},
		testParam("/a", "text/plain", "${/b}", true),
		testParam("/b", "text/plain", "${unknown}", true),
	)
	p := g.get(context.Background(), "/a")
	if p.ContentType != "application/x-template" {
		t.Errorf("mime = %s, want application/x-template (dirty target propagates)", p.ContentType)
	}
	if want := "${unknown}"; p.Value.String != want {
		t.Errorf("value = %s, want %s", p.Value.String, want)
	}
}

func TestCaseBranchFlagPropagation(t *testing.T) {
	nested := `[{"mime":"application/json","template":true,"value":"{\"h\": \"${/vars/host}\"}"}]`
	nestedEncoded, _ := json.Marshal(nested)
	g := testGraph(stubCaseResolver{},
		testParam("/vars", "application/x-null", "", false),
		testParam("/vars/host", "text/plain", "h1", false),
		testParam("/cfg", "application/x-case", `[{"mime":"application/x-case","value":`+string(nestedEncoded)+`}]`, false),
	)
	p := g.get(context.Background(), "/cfg")
	if p.ContentType != "application/json" {
		t.Errorf("mime = %s, want application/json (branch flag rode through two case hops)", p.ContentType)
	}
	if want := `{"h": "h1"}`; p.Value.String != want {
		t.Errorf("value = %s, want %s", p.Value.String, want)
	}
}

func TestDeepMarkCommonExcludesFlagged(t *testing.T) {
	flagged := testParam("/flagged", "text/plain", "${x}", true)
	plain := testParam("/plain", "text/plain", "v", false)
	root := testParam("/", "application/x-null", "", false)
	fp, pp := flagged, plain
	root.Children["flagged"] = &fp
	root.Children["plain"] = &pp

	root.deepMarkCommon(context.Background())
	if flagged.common {
		t.Error("flagged param marked common: it expands per server")
	}
	if !plain.common {
		t.Error("plain param must stay common")
	}
	if root.common {
		t.Error("root with a non-common child must not be common")
	}
}

// The common graph has no server context: a variable that does not resolve
// must leave the value dirty, not panic the tree-sync tick.
func TestCommonGraphTemplateVarFailsSoft(t *testing.T) {
	if _, ok := (commonCaseResolver{}).getTemplateVar("hostname"); ok {
		t.Fatal("commonCaseResolver must resolve no variables")
	}
	g := testGraph(commonCaseResolver{},
		testParam("/cfg", "text/plain", "${hostname}", true),
	)
	p := g.get(context.Background(), "/cfg") // must not panic
	if p.ContentType != "application/x-template" {
		t.Errorf("mime = %s, want application/x-template", p.ContentType)
	}
}

// Every advertised variable must actually resolve — /ui-config serves this
// list so the frontend can classify placeholders.
func TestTemplateVarsMatchGetTemplateVar(t *testing.T) {
	cr := serverCaseResolver{shortname: "short", ip: "127.0.0.1"}
	cr.server.Host = "short.example.com"
	for _, name := range TemplateVars {
		if _, ok := cr.getTemplateVar(name); !ok {
			t.Errorf("advertised variable %q does not resolve", name)
		}
	}
	if _, ok := cr.getTemplateVar("no-such-variable"); ok {
		t.Error("unknown variable resolved unexpectedly")
	}
}
