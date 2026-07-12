//go:build !integration

package admin

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-sql-driver/mysql"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// emitNotification must buffer onto the context sink rather than write to
// notifyDB immediately, so a later rollback cannot leak a phantom notification.
// (notifyDB is nil here; a non-buffered write would dereference it and panic.)
func TestNotificationsAreBufferedUntilCommit(t *testing.T) {
	ctx, sink := withNotifications(context.Background())
	for _, m := range []string{"a", "b", "c"} {
		if err := emitNotification(ctx, m); err != nil {
			t.Fatalf("emitNotification: %v", err)
		}
	}
	if got := len(sink.messages); got != 3 {
		t.Fatalf("buffered %d messages, want 3", got)
	}
}

func TestTemplateEligible(t *testing.T) {
	eligible := []string{"text/plain", "application/x-list", "application/x-server", "application/x-server2", "application/json", "application/x-yaml"}
	ineligible := []string{"application/x-null", "application/x-symlink", "application/x-case", "application/x-template"}
	for _, mime := range eligible {
		if !templateEligible(mime) {
			t.Errorf("%s: want eligible", mime)
		}
	}
	for _, mime := range ineligible {
		if templateEligible(mime) {
			t.Errorf("%s: want ineligible", mime)
		}
	}
}

func TestSubstituteTemplateDummy(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"host": "${/infra/host}"}`, `{"host": "0"}`},
		{`{"port": ${/infra/port}}`, `{"port": 0}`},
		{`${a}-${b}`, `0-0`},
		{`no placeholders`, `no placeholders`},
	}
	for _, tt := range tests {
		if got := substituteTemplateDummy(tt.in); got != tt.want {
			t.Errorf("substituteTemplateDummy(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// validateParameter is exercised only on paths that do not reach the database
// (symlink branches would) — DB is nil in unit tests.
func TestValidateParameterTemplateFlag(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		contentType string
		templated   bool
		value       NullString
		wantErr     error // nil means "must pass"; ErrInvalidValue means "any error"
	}{
		{"flag on text", "text/plain", true, validString("a ${x} b"), nil},
		{"flag on list", "application/x-list", true, validString("${/a},${/b}"), nil},
		{"flag on null", "application/x-null", true, NullString{}, ErrTemplateFlagNotAllowed},
		{"flag on symlink", "application/x-symlink", true, validString("/a"), ErrTemplateFlagNotAllowed},
		{"flag on case", "application/x-case", true, validString("[]"), ErrTemplateFlagNotAllowed},
		{"flag on legacy template", "application/x-template", true, validString("${/a}"), ErrTemplateFlagNotAllowed},
		{"legacy template without flag", "application/x-template", false, validString("${/a}"), nil},

		{"flagged json, placeholder in string position", "application/json", true, validString(`{"host": "${/infra/host}"}`), nil},
		{"flagged json, placeholder in bare position", "application/json", true, validString(`{"port": ${/infra/port}}`), nil},
		{"flagged json, whole document placeholder", "application/json", true, validString(`${/whole}`), nil},
		{"flagged json, broken document", "application/json", true, validString(`{"port": ${/p}`), ErrInvalidValue},
		{"unflagged json with placeholder in bare position", "application/json", false, validString(`{"port": ${/p}}`), ErrInvalidValue},
		{"flagged yaml, bare position", "application/x-yaml", true, validString("port: ${/infra/port}"), nil},

		{"case with flagged json branch", "application/x-case", false,
			validString(`[{"group":"g","mime":"application/json","template":true,"value":"{\"p\": ${/p}}"},{"mime":"text/plain","value":"d"}]`), nil},
		{"case with flag on symlink branch", "application/x-case", false,
			validString(`[{"group":"g","mime":"application/x-symlink","template":true,"value":"/a"}]`), ErrTemplateFlagNotAllowed},
		{"case with stringly-typed branch flag", "application/x-case", false,
			validString(`[{"group":"g","mime":"text/plain","template":"1","value":"v"}]`), ErrInvalidValue},
	}
	for _, tt := range tests {
		err := validateParameter(ctx, tt.contentType, tt.templated, tt.value)
		switch {
		case tt.wantErr == nil && err != nil:
			t.Errorf("%s: unexpected error %v", tt.name, err)
		case tt.wantErr != nil && err == nil:
			t.Errorf("%s: expected an error, got nil", tt.name)
		case errors.Is(tt.wantErr, ErrTemplateFlagNotAllowed) && !errors.Is(err, ErrTemplateFlagNotAllowed):
			t.Errorf("%s: got %v, want ErrTemplateFlagNotAllowed", tt.name, err)
		}
	}
}

func TestIsUnknownColumn(t *testing.T) {
	if !isUnknownColumn(&mysql.MySQLError{Number: 1054, Message: "Unknown column 'Templated' in 'field list'"}) {
		t.Error("error 1054 must be recognized")
	}
	if !isUnknownColumn(fmt.Errorf("scan: %w", &mysql.MySQLError{Number: 1054})) {
		t.Error("a wrapped 1054 must be recognized")
	}
	if isUnknownColumn(&mysql.MySQLError{Number: 1146}) {
		t.Error("other mysql errors must not match")
	}
	if isUnknownColumn(errors.New("connection refused")) {
		t.Error("non-mysql errors must not match")
	}
}

func TestIsPlainText(t *testing.T) {
	plain := &Parameter{ContentType: "text/plain", Value: validString("1")}
	if !isPlainText(plain) {
		t.Error("plain text/plain: want true")
	}
	flagged := &Parameter{ContentType: "text/plain", Templated: true, Value: validString("1")}
	if isPlainText(flagged) {
		t.Error("flagged text/plain: internal readers must treat it as ineligible")
	}
	if isPlainText(nil) {
		t.Error("nil parameter: want false")
	}
	if isPlainText(&Parameter{ContentType: "application/json", Value: validString("1")}) {
		t.Error("wrong type: want false")
	}
}
