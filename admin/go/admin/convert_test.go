//go:build !integration

package admin

import (
	"encoding/json"
	"testing"
)

func TestConvertCaseValueForward(t *testing.T) {
	in := `[
		{"group":"g","mime":"application/x-template","value":"${/tmpl/var}","custom":"kept"},
		{"mime":"application/x-case","value":"[{\"mime\":\"application/x-template\",\"value\":\"${/nested}\"}]"},
		{"mime":"text/plain","value":"untouched"}
	]`
	out, changed, err := convertCaseValue(in, false)
	if err != nil {
		t.Fatalf("convertCaseValue: %v", err)
	}
	if !changed {
		t.Fatal("expected a change")
	}

	var branches []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &branches); err != nil {
		t.Fatalf("output is not valid json: %v", err)
	}
	assertRaw := func(b map[string]json.RawMessage, key, want string) {
		t.Helper()
		if string(b[key]) != want {
			t.Errorf("branch key %s = %s, want %s", key, b[key], want)
		}
	}
	assertRaw(branches[0], "mime", `"text/plain"`)
	assertRaw(branches[0], "template", `true`)
	assertRaw(branches[0], "custom", `"kept"`) // unknown keys survive
	assertRaw(branches[0], "value", `"${/tmpl/var}"`)

	var nested string
	if err := json.Unmarshal(branches[1]["value"], &nested); err != nil {
		t.Fatalf("nested value: %v", err)
	}
	var nestedBranches []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(nested), &nestedBranches); err != nil {
		t.Fatalf("nested case is not valid json: %v", err)
	}
	assertRaw(nestedBranches[0], "mime", `"text/plain"`)
	assertRaw(nestedBranches[0], "template", `true`)

	if string(branches[2]["mime"]) != `"text/plain"` {
		t.Errorf("plain branch mime changed: %s", branches[2]["mime"])
	}
	if _, ok := branches[2]["template"]; ok {
		t.Error("plain branch gained a template key")
	}

	// idempotent: converting the output changes nothing
	if _, changed, err := convertCaseValue(out, false); err != nil || changed {
		t.Errorf("second conversion: changed=%v err=%v, want no-op", changed, err)
	}
}

func TestConvertCaseValueReverseRoundTrip(t *testing.T) {
	legacy := `[{"group":"g","mime":"application/x-template","value":"${/x}"}]`
	flagged, changed, err := convertCaseValue(legacy, false)
	if err != nil || !changed {
		t.Fatalf("forward: changed=%v err=%v", changed, err)
	}
	back, changed, err := convertCaseValue(flagged, true)
	if err != nil || !changed {
		t.Fatalf("reverse: changed=%v err=%v", changed, err)
	}
	var branches []caseBranch
	if err := json.Unmarshal([]byte(back), &branches); err != nil {
		t.Fatalf("reverse output: %v", err)
	}
	if branches[0].ContentType.String != "application/x-template" || branches[0].Templated {
		t.Errorf("reverse did not restore the legacy branch: %+v", branches[0])
	}
}

func TestCaseHasTypedTemplatedBranch(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"flagged text branch is lossless", `[{"mime":"text/plain","template":true,"value":"${/x}"}]`, false},
		{"flagged json branch is a demotion", `[{"mime":"application/json","template":true,"value":"{}"}]`, true},
		{"nested flagged json branch", `[{"mime":"application/x-case","value":"[{\"mime\":\"application/json\",\"template\":true,\"value\":\"{}\"}]"}]`, true},
		{"legacy template branch", `[{"mime":"application/x-template","value":"${/x}"}]`, false},
	}
	for _, tt := range tests {
		if got := caseHasTypedTemplatedBranch(tt.value); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
