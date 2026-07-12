package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/rs/zerolog/log"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// ConvertTemplateTypes is the one-shot data migration behind
// `onlineconf-admin -convert-template-types`: it rewrites every live legacy
// application/x-template parameter — node rows and case branches alike, at
// any nesting — to the flag form (text/plain, Templated), or back with
// reverse. Deliberately operator-run only: each touched parameter gets a
// version bump and a Silent change-log row (no bot or notifyDB noise), so
// the moment is a human's choice.
//
// The reverse direction is lossy for parameters that already use the new
// capability: a flagged json/yaml (or list/server) value has no legacy
// equivalent, and demoting it to application/x-template changes the type
// its consumers see. Such parameters are listed and refused without force.
//
// Runs under an advisory lock, refuses to run while the template flag is
// opted out (the fleet may still contain old binaries — converting would
// make them serve literal ${...} text), and is idempotent: a re-run
// converges to zero conversions. Dependency edges are not touched: both
// encodings of the same value produce identical edge sets by design.
func ConvertTemplateTypes(ctx context.Context, reverse, force bool) error {
	optedOut, err := templateFlagOptedOut(ctx)
	if err != nil {
		return err
	}
	if optedOut {
		return fmt.Errorf("the template flag is opted out (%s); lift the opt-out after the whole fleet runs a flag-aware binary", templateFlagPath)
	}

	release, err := acquireAdvisoryLock(ctx, "template-convert", depsRebuildLockWait)
	if err != nil {
		return err
	}
	defer release()

	if reverse {
		demoted, err := scanReverseDemotions(ctx)
		if err != nil {
			return err
		}
		if len(demoted) > 0 && !force {
			return fmt.Errorf("reverse conversion would demote %d typed templated parameter(s) to untyped application/x-template, changing the type their consumers see: %s (re-run with -force to accept)",
				len(demoted), strings.Join(demoted, ", "))
		}
	}

	nodes, err := convertNodeRows(ctx, reverse)
	if err != nil {
		return err
	}
	cases, err := convertCaseRows(ctx, reverse)
	if err != nil {
		return err
	}

	if !reverse {
		if err := verifyConverted(ctx); err != nil {
			return err
		}
	}

	logNamespaceCounts(append(nodes, cases...))
	log.Info().Bool("reverse", reverse).Int("nodes", len(nodes)).Int("cases", len(cases)).
		Msg("template conversion finished")
	return nil
}

const convertComment = "template content type converted to the template flag"
const convertReverseComment = "template flag converted back to the template content type"

// templateFlagOptedOut reads the opt-out parameter directly, bypassing the
// ACL-filtered SelectParameter: the tool runs with no authenticated user, and
// the access function would blank the value, silently passing the check.
// Unlike the request-path isTemplateFlagEnabled it does not follow symlinks:
// the opt-out is created as a plain parameter, and an aliased one is not
// worth the extra resolution machinery here.
func templateFlagOptedOut(ctx context.Context) (bool, error) {
	row := DB.QueryRowContext(ctx,
		"SELECT ContentType, Templated, Value FROM my_config_tree WHERE Path = ? AND NOT Deleted", templateFlagPath)
	var contentType string
	var templated bool
	var value sql.NullString
	err := row.Scan(&contentType, &templated, &value)
	if err == sql.ErrNoRows {
		return false, nil // absent means enabled
	} else if err != nil {
		return false, err
	}
	if contentType != "text/plain" || templated || !value.Valid {
		return false, nil // not a usable opt-out value: treated as absent
	}
	return value.String == "" || value.String == "0", nil
}

// scanReverseDemotions lists the live parameters whose reverse conversion is
// lossy: flagged values of any type but text/plain, at the node level or
// inside a case branch.
func scanReverseDemotions(ctx context.Context) ([]string, error) {
	rows, err := DB.QueryContext(ctx, `
		SELECT Path FROM my_config_tree
		 WHERE NOT Deleted AND Templated AND ContentType <> 'text/plain'
		 ORDER BY Path
	`)
	if err != nil {
		return nil, err
	}
	var demoted []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return nil, err
		}
		demoted = append(demoted, path)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	rows, err = DB.QueryContext(ctx, `
		SELECT Path, Value FROM my_config_tree
		 WHERE NOT Deleted AND ContentType = 'application/x-case' AND Value LIKE '%template%'
		 ORDER BY Path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path string
		var value sql.NullString
		if err := rows.Scan(&path, &value); err != nil {
			return nil, err
		}
		if value.Valid && caseHasTypedTemplatedBranch(value.String) {
			demoted = append(demoted, path)
		}
	}
	return demoted, rows.Err()
}

func caseHasTypedTemplatedBranch(value string) bool {
	var branches []caseBranch
	if err := json.Unmarshal([]byte(value), &branches); err != nil {
		return false
	}
	for _, b := range branches {
		if b.Templated && b.ContentType.String != "text/plain" {
			return true
		}
		if b.ContentType.String == "application/x-case" && b.Value.Valid && caseHasTypedTemplatedBranch(b.Value.String) {
			return true
		}
	}
	return false
}

// convertNodeRows rewrites the matching node-level parameters one row per
// transaction, guarded by FOR UPDATE so a concurrent edit cannot be
// clobbered, and logs each as a Silent version bump.
func convertNodeRows(ctx context.Context, reverse bool) ([]string, error) {
	where := "ContentType = 'application/x-template'"
	if reverse {
		where = "Templated"
	}
	ids, err := selectIDs(ctx, "SELECT ID FROM my_config_tree WHERE NOT Deleted AND "+where+" ORDER BY ID")
	if err != nil {
		return nil, err
	}

	var converted []string
	for _, id := range ids {
		path, changed, err := convertOneNode(ctx, id, reverse)
		if err != nil {
			return converted, err
		}
		if changed {
			converted = append(converted, path)
		}
	}
	return converted, nil
}

func convertOneNode(ctx context.Context, id int, reverse bool) (string, bool, error) {
	tx, err := DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, "SELECT Path, ContentType, Templated FROM my_config_tree WHERE ID = ? AND NOT Deleted FOR UPDATE", id)
	var path, contentType string
	var templated bool
	err = row.Scan(&path, &contentType, &templated)
	if err == sql.ErrNoRows {
		return "", false, nil // gone since the scan; nothing to convert
	} else if err != nil {
		return "", false, err
	}

	comment := convertComment
	var newType string
	var newTemplated bool
	switch {
	case !reverse && contentType == "application/x-template":
		newType, newTemplated = "text/plain", true
	case reverse && templated:
		// text/plain converts losslessly; anything else is the demotion the
		// caller has already listed and force-acknowledged
		newType, newTemplated = "application/x-template", false
		comment = convertReverseComment
	default:
		return "", false, nil // already converted by a concurrent run
	}

	_, err = tx.ExecContext(ctx,
		"UPDATE my_config_tree SET ContentType = ?, Templated = ?, Version = Version + 1, MTime = now() WHERE ID = ?",
		newType, newTemplated, id)
	if err == nil {
		err = logSilentConversion(ctx, tx, id, comment)
	}
	if err != nil {
		return "", false, err
	}
	return path, true, tx.Commit()
}

// convertCaseRows rewrites template branches inside case values (nested
// cases included), preserving branch keys it does not know about.
func convertCaseRows(ctx context.Context, reverse bool) ([]string, error) {
	// the LIKE prefilter is an optimization only: the decode decides
	ids, err := selectIDs(ctx, `
		SELECT ID FROM my_config_tree
		 WHERE NOT Deleted AND ContentType = 'application/x-case' AND Value LIKE '%template%'
		 ORDER BY ID
	`)
	if err != nil {
		return nil, err
	}

	var converted []string
	for _, id := range ids {
		path, changed, err := convertOneCase(ctx, id, reverse)
		if err != nil {
			return converted, err
		}
		if changed {
			converted = append(converted, path)
		}
	}
	return converted, nil
}

func convertOneCase(ctx context.Context, id int, reverse bool) (string, bool, error) {
	tx, err := DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx, "SELECT Path, Value FROM my_config_tree WHERE ID = ? AND NOT Deleted AND ContentType = 'application/x-case' FOR UPDATE", id)
	var path string
	var value sql.NullString
	err = row.Scan(&path, &value)
	if err == sql.ErrNoRows {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	if !value.Valid {
		return "", false, nil
	}

	newValue, changed, err := convertCaseValue(value.String, reverse)
	if err != nil {
		log.Warn().Str("path", path).Err(err).Msg("skipping case parameter with undecodable value")
		return "", false, nil
	}
	if !changed {
		return "", false, nil
	}

	comment := convertComment
	if reverse {
		comment = convertReverseComment
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE my_config_tree SET Value = ?, Version = Version + 1, MTime = now() WHERE ID = ?",
		newValue, id)
	if err == nil {
		err = logSilentConversion(ctx, tx, id, comment)
	}
	if err != nil {
		return "", false, err
	}
	return path, true, tx.Commit()
}

// convertCaseValue rewrites the template branches of one case value. Unknown
// branch keys survive the round-trip (branches decode into raw-message maps,
// not typed structs); key order inside a branch object is not preserved —
// JSON object order is not significant, and the resolver decodes by key.
func convertCaseValue(value string, reverse bool) (string, bool, error) {
	var branches []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &branches); err != nil {
		return "", false, err
	}
	changed := false
	for _, b := range branches {
		var mime string
		if raw, ok := b["mime"]; ok {
			if err := json.Unmarshal(raw, &mime); err != nil {
				continue
			}
		}
		if mime == "application/x-case" {
			var nested string
			if raw, ok := b["value"]; ok && json.Unmarshal(raw, &nested) == nil {
				newNested, c, err := convertCaseValue(nested, reverse)
				if err != nil || !c {
					continue
				}
				enc, err := json.Marshal(newNested)
				if err != nil {
					continue
				}
				b["value"] = enc
				changed = true
			}
			continue
		}
		if !reverse && mime == "application/x-template" {
			b["mime"] = json.RawMessage(`"text/plain"`)
			b["template"] = json.RawMessage(`true`)
			changed = true
		} else if reverse {
			var templated bool
			if raw, ok := b["template"]; ok {
				json.Unmarshal(raw, &templated)
			}
			if templated {
				b["mime"] = json.RawMessage(`"application/x-template"`)
				delete(b, "template")
				changed = true
			}
		}
	}
	if !changed {
		return value, false, nil
	}
	out, err := json.Marshal(branches)
	if err != nil {
		return "", false, err
	}
	return string(out), true, nil
}

func logSilentConversion(ctx context.Context, tx *sql.Tx, id int, comment string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO my_config_tree_log (NodeID, Path, Version, Value, ContentType, Templated, Author, MTime, Comment, Deleted, Silent)
		SELECT ID, Path, Version, Value, ContentType, Templated, 'onlineconf', MTime, ?, Deleted, true
		FROM my_config_tree
		WHERE ID = ?
	`, comment, id)
	return err
}

// verifyConverted asserts the forward conversion converged: no live legacy
// template rows remain, at the node level or textually inside case values.
func verifyConverted(ctx context.Context) error {
	var nodes, cases int
	row := DB.QueryRowContext(ctx, "SELECT count(*) FROM my_config_tree WHERE NOT Deleted AND ContentType = 'application/x-template'")
	if err := row.Scan(&nodes); err != nil {
		return err
	}
	row = DB.QueryRowContext(ctx, "SELECT count(*) FROM my_config_tree WHERE NOT Deleted AND ContentType = 'application/x-case' AND Value LIKE '%application/x-template%'")
	if err := row.Scan(&cases); err != nil {
		return err
	}
	if nodes != 0 || cases != 0 {
		return fmt.Errorf("conversion left %d legacy template node(s) and %d case value(s) mentioning the legacy type", nodes, cases)
	}
	return nil
}

func selectIDs(ctx context.Context, query string) ([]int, error) {
	rows, err := DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func logNamespaceCounts(paths []string) {
	if len(paths) == 0 {
		return
	}
	counts := make(map[string]int)
	for _, path := range paths {
		ns := "/"
		if segments := strings.SplitN(path, "/", 3); len(segments) > 1 && segments[1] != "" {
			ns = "/" + segments[1]
		}
		counts[ns]++
	}
	namespaces := make([]string, 0, len(counts))
	for ns := range counts {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		log.Info().Str("namespace", ns).Int("parameters", counts[ns]).Msg("converted")
	}
}
