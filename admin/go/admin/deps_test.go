//go:build !integration

package admin

import (
	"reflect"
	"testing"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

func validString(s string) NullString {
	var v NullString
	v.Valid = true
	v.String = s
	return v
}

func TestReferrerTargets(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		templated   bool
		value       NullString
		want        []string
	}{{
		"symlink",
		"application/x-symlink",
		false,
		validString("/a/b"),
		[]string{"/a/b"},
	}, {
		"template with path vars and a context var",
		"application/x-template",
		false,
		validString("${/infra/host}:${port}/${/infra/base}"),
		[]string{"/infra/host", "/infra/base"},
	}, {
		"flagged value with path vars and a context var",
		"application/json",
		true,
		validString(`{"host": "${/infra/host}", "port": ${port}, "base": "${/infra/base}"}`),
		[]string{"/infra/host", "/infra/base"},
	}, {
		"template without path vars",
		"application/x-template",
		false,
		validString("plain ${hostname} text"),
		nil,
	}, {
		"unflagged value with template-looking text",
		"text/plain",
		false,
		validString("${/not/a/referrer}"),
		nil,
	}, {
		"case with symlink, template and plain branches",
		"application/x-case",
		false,
		validString(`[
			{"mime":"application/x-symlink","value":"/by/group","group":"g"},
			{"mime":"application/x-template","value":"${/tmpl/var}","datacenter":"dc"},
			{"mime":"text/plain","value":"default"}
		]`),
		[]string{"/by/group", "/tmpl/var"},
	}, {
		"case with a flagged branch",
		"application/x-case",
		false,
		validString(`[
			{"mime":"application/json","template":true,"value":"{\"h\":\"${/tmpl/var}\"}","datacenter":"dc"},
			{"mime":"text/plain","value":"default"}
		]`),
		[]string{"/tmpl/var"},
	}, {
		"nested case",
		"application/x-case",
		false,
		validString(`[{"mime":"application/x-case","value":"[{\"mime\":\"application/x-symlink\",\"value\":\"/nested\"}]"}]`),
		[]string{"/nested"},
	}, {
		"nested case with a flagged branch",
		"application/x-case",
		false,
		validString(`[{"mime":"application/x-case","value":"[{\"mime\":\"text\/plain\",\"template\":true,\"value\":\"${/nested/tmpl}\"}]"}]`),
		[]string{"/nested/tmpl"},
	}, {
		"invalid case json",
		"application/x-case",
		false,
		validString("not json"),
		nil,
	}, {
		"plain value",
		"text/plain",
		false,
		validString("/looks/like/a/path"),
		nil,
	}, {
		"null value",
		"application/x-symlink",
		false,
		NullString{},
		nil,
	}}
	for _, tt := range tests {
		if got := referrerTargets(tt.contentType, tt.templated, tt.value); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The legacy template encoding and the flag encoding of the same value must
// produce identical dependency edges — the data conversion relies on this to
// leave the dep tables untouched.
func TestReferrerTargetsEncodingInvariance(t *testing.T) {
	values := []string{
		"${/infra/host}:${port}",
		"no path vars at all ${hostname}",
		"${/a}${/b}${/a}",
	}
	for _, v := range values {
		legacy := referrerTargets("application/x-template", false, validString(v))
		flagged := referrerTargets("text/plain", true, validString(v))
		if !reflect.DeepEqual(legacy, flagged) {
			t.Errorf("encodings diverge for %q: legacy %v, flagged %v", v, legacy, flagged)
		}
	}

	legacyCase := validString(`[{"mime":"application/x-template","value":"${/tmpl/var}","group":"g"}]`)
	flaggedCase := validString(`[{"mime":"text/plain","template":true,"value":"${/tmpl/var}","group":"g"}]`)
	legacy := referrerTargets("application/x-case", false, legacyCase)
	flagged := referrerTargets("application/x-case", false, flaggedCase)
	if !reflect.DeepEqual(legacy, flagged) {
		t.Errorf("case branch encodings diverge: legacy %v, flagged %v", legacy, flagged)
	}
}

func TestCaseSymlinkTargets(t *testing.T) {
	value := validString(`[
		{"mime":"application/x-symlink","value":"/one"},
		{"mime":"application/x-template","value":"${/not/a/redirect}"},
		{"mime":"application/x-case","value":"[{\"mime\":\"application/x-symlink\",\"value\":\"/two\"}]"},
		{"mime":"text/plain","value":"/three"}
	]`)
	want := []string{"/one", "/two"}
	if got := caseSymlinkTargets(value); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDepsNeedRebuild(t *testing.T) {
	tests := []struct {
		name        string
		hasStored   bool
		storedDepth int
		configDepth int
		want        bool
	}{
		{"fresh / never built", false, 0, 10, true},
		{"unchanged", true, 10, 10, false},
		{"raised", true, 10, 12, true},
		{"lowered keeps deeper edges", true, 10, 8, false},
	}
	for _, tt := range tests {
		if got := depsNeedRebuild(tt.hasStored, tt.storedDepth, tt.configDepth); got != tt.want {
			t.Errorf("%s: depsNeedRebuild(%v, %d, %d) = %v, want %v",
				tt.name, tt.hasStored, tt.storedDepth, tt.configDepth, got, tt.want)
		}
	}
}

func TestDepsRedirectChanged(t *testing.T) {
	tests := []struct {
		oldType, newType string
		want             bool
	}{
		{"text/plain", "text/plain", false},
		{"application/x-template", "application/x-template", false},
		{"text/plain", "application/x-symlink", true},
		{"application/x-symlink", "text/plain", true},
		{"application/x-symlink", "application/x-symlink", true},
		{"application/x-case", "application/x-case", true},
	}
	for _, tt := range tests {
		if got := depsRedirectChanged(tt.oldType, tt.newType); got != tt.want {
			t.Errorf("depsRedirectChanged(%q, %q) = %v, want %v", tt.oldType, tt.newType, got, tt.want)
		}
	}
}
