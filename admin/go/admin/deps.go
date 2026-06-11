package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

const defaultSymlinkDepth = 10

// depthParamPath holds the configured maximum symlink depth as a plain-text
// integer, alongside the disable switch. Changing it takes effect without a
// restart (see reconcileDepsDepth).
const depthParamPath = "/onlineconf/deleted-key-symlinks-check-depth"

// maxSymlinkDepth bounds how many symlink/case hops the edge walk follows when
// computing a referrer's dependency edges; it never affects the O(1) deletion
// check. It mirrors depthParamPath, refreshed at startup and whenever that
// parameter is written. Read concurrently by edge walks, so kept atomic.
var maxSymlinkDepth atomic.Int64

func init() {
	maxSymlinkDepth.Store(defaultSymlinkDepth)
}

// Dependency tracking of symlink/template/case parameters (referrers).
//
// For every referrer my_config_tree_dep holds one edge per node its resolution
// depends on: every symlink (or symlink case branch) node followed on the way
// plus the terminal node of each target path. Deleting any of these nodes
// would break the referrer, so DeleteParameter refuses to delete a node with
// incoming edges from live referrers — a single indexed lookup instead of the
// recursive referrer search it replaces.
//
// Intermediate plain nodes on a resolution path carry no edges: they are
// protected transitively — a non-empty node cannot be deleted (ErrNotEmpty)
// and the chain ends at the terminal edge.
//
// The edges are maintained inside the same transaction as the tree write that
// invalidates them; see the calls in parameter.go.

// templateVarRe matches absolute-path ${/...} template variables; non-path
// variables (e.g. ${hostname}) resolve from the server context, not the tree.
var templateVarRe = regexp.MustCompile(`\$\{(/[^}]*)\}`)

type caseEntry struct {
	ContentType string     `json:"mime"`
	Value       NullString `json:"value"`
}

// referrerTargets extracts the tree paths a parameter value refers to: the
// target of a symlink, the ${/path} variables of a template, and the same
// from every (possibly nested) case branch.
func referrerTargets(contentType string, value NullString) []string {
	if !value.Valid {
		return nil
	}
	switch contentType {
	case "application/x-symlink":
		return []string{value.String}
	case "application/x-template":
		return templatePathVars(value.String)
	case "application/x-case":
		var cases []caseEntry
		if err := json.Unmarshal([]byte(value.String), &cases); err != nil {
			return nil
		}
		var targets []string
		for _, c := range cases {
			targets = append(targets, referrerTargets(c.ContentType, c.Value)...)
		}
		return targets
	}
	return nil
}

func templatePathVars(value string) []string {
	matches := templateVarRe.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return nil
	}
	vars := make([]string, 0, len(matches))
	for _, m := range matches {
		vars = append(vars, m[1])
	}
	return vars
}

// caseSymlinkTargets returns the symlink branch values of a case parameter:
// the paths through which resolution may continue when the case is met in the
// middle of a path. Template branches do not redirect resolution.
func caseSymlinkTargets(value NullString) []string {
	if !value.Valid {
		return nil
	}
	var cases []caseEntry
	if err := json.Unmarshal([]byte(value.String), &cases); err != nil {
		return nil
	}
	var targets []string
	for _, c := range cases {
		switch c.ContentType {
		case "application/x-symlink":
			if c.Value.Valid {
				targets = append(targets, c.Value.String)
			}
		case "application/x-case":
			targets = append(targets, caseSymlinkTargets(c.Value)...)
		}
	}
	return targets
}

type depNode struct {
	ID          int
	ContentType string
	Value       NullString
}

func selectDepNode(ctx context.Context, tx *sql.Tx, path string) (*depNode, error) {
	row := tx.QueryRowContext(ctx, "SELECT ID, ContentType, Value FROM my_config_tree WHERE Path = ? AND NOT Deleted", path)
	var n depNode
	err := row.Scan(&n.ID, &n.ContentType, &n.Value)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &n, nil
}

// depWalker simulates resolution of target paths against the current tree the
// way the resolver does (graph.get): segment by segment, following symlinks —
// and, lacking a server context, every symlink branch of a case — before
// descending. It collects the IDs of the nodes the resolution depends on.
// Cycles are cut by the visited set, chains by maxSymlinkDepth, and dangling
// paths simply end the walk: a reference that does not resolve depends on
// nothing (it is healed if its target appears later, see
// healReferrersMentioning).
type depWalker struct {
	ctx     context.Context
	tx      *sql.Tx
	deps    map[int]bool
	visited map[string]bool
}

func (w *depWalker) walk(path string, depth int) error {
	if int64(depth) > maxSymlinkDepth.Load() || len(path) > maxPathLen || w.visited[path] {
		return nil
	}
	w.visited[path] = true
	if path == "/" {
		root, err := selectDepNode(w.ctx, w.tx, "/")
		if err != nil || root == nil {
			return err
		}
		w.deps[root.ID] = true
		return nil
	}
	if !strings.HasPrefix(path, "/") {
		return nil
	}
	segments := strings.Split(path[1:], "/")
	current := ""
	for i, segment := range segments {
		if segment == "" { // malformed: //, trailing /
			return nil
		}
		current += "/" + segment
		node, err := selectDepNode(w.ctx, w.tx, current)
		if err != nil {
			return err
		}
		if node == nil { // dangling
			return nil
		}
		if i == len(segments)-1 {
			w.deps[node.ID] = true // the terminal
			return nil
		}
		rest := "/" + strings.Join(segments[i+1:], "/")
		switch node.ContentType {
		case "application/x-symlink":
			w.deps[node.ID] = true
			if !node.Value.Valid {
				return nil
			}
			return w.walk(node.Value.String+rest, depth+1)
		case "application/x-case":
			for _, branch := range caseSymlinkTargets(node.Value) {
				w.deps[node.ID] = true
				if err := w.walk(branch+rest, depth+1); err != nil {
					return err
				}
			}
			// resolution may also continue into the case node's own tree
			// children (the resolver takes children from the tree), so keep
			// descending
		}
	}
	return nil
}

// updateNodeDeps recomputes and stores the dependency edges of a node.
// Self-edges are skipped: a referrer resolving through or onto itself must
// not block its own deletion.
func updateNodeDeps(ctx context.Context, tx *sql.Tx, id int, contentType string, value NullString) error {
	w := depWalker{ctx: ctx, tx: tx, deps: map[int]bool{}, visited: map[string]bool{}}
	for _, target := range referrerTargets(contentType, value) {
		if err := w.walk(target, 0); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM my_config_tree_dep WHERE ReferrerID = ?", id); err != nil {
		return err
	}
	delete(w.deps, id)
	if len(w.deps) == 0 {
		return nil
	}
	query := strings.Builder{}
	query.WriteString("INSERT INTO my_config_tree_dep (ReferrerID, TargetID) VALUES ")
	bind := make([]interface{}, 0, 2*len(w.deps))
	for target := range w.deps {
		if len(bind) > 0 {
			query.WriteString(", ")
		}
		query.WriteString("(?, ?)")
		bind = append(bind, id, target)
	}
	_, err := tx.ExecContext(ctx, query.String(), bind...)
	return err
}

// updateParameterDepsByPath recomputes the edges of the node currently at the
// given path (used where the caller has no node ID at hand, e.g. right after
// an INSERT).
func updateParameterDepsByPath(ctx context.Context, tx *sql.Tx, path, contentType string, value NullString) error {
	row := tx.QueryRowContext(ctx, "SELECT ID FROM my_config_tree WHERE Path = ?", path)
	var id int
	if err := row.Scan(&id); err != nil {
		return err
	}
	return updateNodeDeps(ctx, tx, id, contentType, value)
}

// depsRedirectChanged reports whether a value change of a node can alter how
// OTHER referrers resolve through it: either its content type changed (e.g. a
// plain terminal became a symlink) or it is a redirecting node (symlink or
// case) whose targets may have changed. A plain value edit never does.
func depsRedirectChanged(oldType, newType string) bool {
	if oldType != newType {
		return true
	}
	return newType == "application/x-symlink" || newType == "application/x-case"
}

// recomputeMovedDependents refreshes referrers that depend on any node of a
// subtree just moved to newPath: their textual targets still point at the old
// location, so they may now dangle or resolve elsewhere.
func recomputeMovedDependents(ctx context.Context, tx *sql.Tx, newPath string) error {
	rows, err := tx.QueryContext(ctx, "SELECT ID FROM my_config_tree WHERE Path = ? OR Path LIKE ?",
		newPath, likeEscape(newPath)+"/%")
	if err != nil {
		return err
	}
	var movedIDs []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		movedIDs = append(movedIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close() // before reusing the connection below
	dependents, err := selectDependentIDs(ctx, tx, movedIDs)
	if err != nil {
		return err
	}
	return recomputeDeps(ctx, tx, dependents)
}

// recomputeDeps refreshes the edges of the given referrers (skipping ones
// that are gone or deleted by now).
func recomputeDeps(ctx context.Context, tx *sql.Tx, referrerIDs []int) error {
	for _, id := range referrerIDs {
		row := tx.QueryRowContext(ctx, "SELECT ID, ContentType, Value FROM my_config_tree WHERE ID = ? AND NOT Deleted", id)
		var n depNode
		err := row.Scan(&n.ID, &n.ContentType, &n.Value)
		if err == sql.ErrNoRows {
			continue
		} else if err != nil {
			return err
		}
		if err := updateNodeDeps(ctx, tx, n.ID, n.ContentType, n.Value); err != nil {
			return err
		}
	}
	return nil
}

// selectDependentIDs returns the referrers that depend on any of the given
// nodes. Because a referrer has an edge to every hop of its resolution path,
// one level is exhaustive — no transitive search is needed.
func selectDependentIDs(ctx context.Context, tx *sql.Tx, targetIDs []int) ([]int, error) {
	if len(targetIDs) == 0 {
		return nil, nil
	}
	marks := make([]string, len(targetIDs))
	bind := make([]interface{}, len(targetIDs))
	for i, id := range targetIDs {
		marks[i] = "?"
		bind[i] = id
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT DISTINCT ReferrerID FROM my_config_tree_dep WHERE TargetID IN ("+strings.Join(marks, ", ")+")",
		bind...)
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

// healReferrersMentioning recomputes the edges of referrers that textually
// mention the given path or its subtree. It must be called when a path starts
// to exist (create, resurrect, move-in): a referrer written while its target
// was missing carries no edge to it (templates may legally reference
// not-yet-existing paths, and symlinks can dangle after moves or
// check-disabled periods), so without healing the new node would be deletable
// while referenced.
func healReferrersMentioning(ctx context.Context, tx *sql.Tx, path string) error {
	exact := path
	prefix := likeEscape(path) + "/%"
	tmplExact := "%${" + likeEscape(path) + "}%"
	tmplPrefix := "%${" + likeEscape(path) + "/%"
	// Case values are matched by plain containment: branch values stay literal
	// in the JSON at any nesting depth, so the match covers nested cases too.
	// Over-matching is harmless — the recompute below extracts targets
	// precisely and recording no edge is a no-op.
	contains := "%" + likeEscape(path) + "%"
	rows, err := tx.QueryContext(ctx, `
		SELECT ID
		  FROM my_config_tree
		 WHERE ContentType = 'application/x-symlink'
		   AND NOT Deleted
		   AND (Value = ? OR Value LIKE ?)
		UNION
		SELECT ID
		  FROM my_config_tree
		 WHERE ContentType = 'application/x-template'
		   AND NOT Deleted
		   AND (Value LIKE ? OR Value LIKE ?)
		UNION
		SELECT ID
		  FROM my_config_tree
		 WHERE ContentType = 'application/x-case'
		   AND NOT Deleted
		   AND Value LIKE ?
	`, exact, prefix, tmplExact, tmplPrefix, contains)
	if err != nil {
		return err
	}
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close() // before reusing the connection in recomputeDeps
	return recomputeDeps(ctx, tx, ids)
}

// checkParameterReferrers refuses the deletion of a parameter some live
// referrer depends on. Self-edges never exist (see updateNodeDeps), but the
// referrer may have been excluded there under a different ID lifecycle, so the
// check guards against them too.
func checkParameterReferrers(ctx context.Context, tx *sql.Tx, p *Parameter) error {
	if p.Path == "/" {
		return nil
	}
	disabled, err := isDeletedParamSymlinksCheckDisabled(ctx)
	if err != nil {
		return err
	}
	if disabled {
		return nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT t.Path, t.ContentType
		  FROM my_config_tree_dep d
		  JOIN my_config_tree t ON t.ID = d.ReferrerID
		 WHERE d.TargetID = ? AND d.ReferrerID <> ? AND NOT t.Deleted
		 ORDER BY t.Path
	`, p.ID, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	var symlinked, expanded []string
	for rows.Next() {
		var path, contentType string
		if err := rows.Scan(&path, &contentType); err != nil {
			return err
		}
		if contentType == "application/x-template" {
			expanded = append(expanded, path)
		} else {
			symlinked = append(symlinked, path)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(symlinked) != 0 {
		return fmt.Errorf("%w: %s", ErrDeletedSymlink, strings.Join(symlinked, ", "))
	}
	if len(expanded) != 0 {
		return fmt.Errorf("%w: %s", ErrDeletedTmpl, strings.Join(expanded, ", "))
	}
	return nil
}

// depsRebuildTimeout bounds a full edge rebuild. A walk over a huge configured
// depth can take arbitrarily long; rather than let it run unbounded (off the
// request path it would otherwise never give up), abandon it after this and
// tell the operator to lower the depth. Generous because the rebuild no longer
// blocks any request.
var depsRebuildTimeout = 5 * time.Minute

// depsRebuildMu serializes full rebuilds so concurrent depth changes (or a
// startup rebuild overlapping a write-triggered one) do not race on the table.
var depsRebuildMu sync.Mutex

// rebuildDepsBounded performs a needed rebuild at the given depth, serialized
// and bounded by depsRebuildTimeout, logging the outcome. On timeout (typically
// an excessive depth) it tells the operator to lower depthParamPath. Walks read
// the current maxSymlinkDepth, which callers set to depth beforehand.
func rebuildDepsBounded(depth int) error {
	depsRebuildMu.Lock()
	defer depsRebuildMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), depsRebuildTimeout)
	defer cancel()

	count, err := rebuildDepsIfNeeded(ctx, depth)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		log.Error().Int("depth", depth).Dur("timeout", depsRebuildTimeout).Msgf(
			"parameter dependency rebuild timed out; lower %s", depthParamPath)
	case err != nil:
		log.Error().Err(err).Msg("failed to rebuild parameter dependencies")
	case count >= 0:
		log.Info().Int("referrers", count).Int("depth", depth).Msg("rebuilt parameter dependencies")
	}
	return err
}

// initializeDeps reconciles the dependency table with the configured depth on
// startup. The rebuild runs synchronously (so existing referrers are protected
// before the server serves requests) but is bounded by depsRebuildTimeout, and
// the depth read retries while the database is not yet reachable at boot (the
// resolver has the same problem and the same remedy).
func initializeDeps() {
	ctx := context.Background()
	apply := func() bool {
		depth, err := deletedParamSymlinksCheckDepth(ctx)
		if err != nil {
			log.Error().Err(err).Msg("failed to read symlink dependency depth")
			return false // database not ready yet; retry
		}
		maxSymlinkDepth.Store(int64(depth))
		// retry transient rebuild errors, but not a timeout — it would only
		// time out again; the operator is told to lower the depth instead.
		if err := rebuildDepsBounded(depth); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return true
	}
	if apply() {
		return
	}
	go func() {
		defer RecoverPanic("dependency backfill")
		for !apply() {
			time.Sleep(5 * time.Second)
		}
	}()
}

// reconcileDepthIfChanged applies a change to depthParamPath: it refreshes the
// effective depth synchronously (cheap) and kicks off any needed rebuild in the
// background, so setting a very high depth does not freeze the request that set
// it. Called by the write paths after they commit.
func reconcileDepthIfChanged(ctx context.Context, paths ...string) {
	for _, p := range paths {
		if p != depthParamPath {
			continue
		}
		depth, err := deletedParamSymlinksCheckDepth(ctx)
		if err != nil {
			log.Ctx(ctx).Error().Err(err).Msg("failed to read symlink dependency depth")
			return
		}
		maxSymlinkDepth.Store(int64(depth))
		go func() {
			defer RecoverPanic("dependency rebuild")
			rebuildDepsBounded(depth)
		}()
		return
	}
}

// deletedParamSymlinksCheckDepth reads the configured maximum symlink depth from
// the tree, falling back to defaultSymlinkDepth when unset or invalid.
func deletedParamSymlinksCheckDepth(ctx context.Context) (int, error) {
	param, err := SelectParameterResolvingSymlink(ctx, depthParamPath)
	if err != nil {
		return 0, err
	}
	if param == nil || param.ContentType != "text/plain" || !param.Value.Valid {
		return defaultSymlinkDepth, nil
	}
	depth, err := strconv.Atoi(strings.TrimSpace(param.Value.String))
	if err != nil || depth <= 0 {
		return defaultSymlinkDepth, nil
	}
	return depth, nil
}

// depsNeedRebuild decides whether the edge table must be rebuilt for the given
// configured depth. A rebuild is needed when the table was never built (fresh
// install / first start after migration) or when the depth was raised above
// the depth it was last built at; lowering needs none.
func depsNeedRebuild(hasStored bool, storedDepth, configDepth int) bool {
	return !hasStored || configDepth > storedDepth
}

func selectDepDepth(ctx context.Context, tx *sql.Tx) (int, bool, error) {
	row := tx.QueryRowContext(ctx, "SELECT Depth FROM my_config_tree_dep_meta LIMIT 1")
	var depth int
	err := row.Scan(&depth)
	if err == sql.ErrNoRows {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	return depth, true, nil
}

// rebuildDepsIfNeeded recomputes every referrer's edges at the given depth when
// depsNeedRebuild says so, and records that depth. Returns the number of
// referrers rebuilt, or -1 when the table was already up to date.
func rebuildDepsIfNeeded(ctx context.Context, depth int) (int, error) {
	tx, err := DB.BeginTx(ctx, nil)
	if err != nil {
		return -1, err
	}
	defer tx.Rollback()

	storedDepth, hasStored, err := selectDepDepth(ctx, tx)
	if err != nil {
		return -1, err
	}
	if !depsNeedRebuild(hasStored, storedDepth, depth) {
		return -1, nil
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM my_config_tree_dep"); err != nil {
		return -1, err
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT ID, ContentType, Value
		  FROM my_config_tree
		 WHERE NOT Deleted
		   AND ContentType IN ('application/x-symlink', 'application/x-template', 'application/x-case')
	`)
	if err != nil {
		return -1, err
	}
	referrers := make([]depNode, 0)
	for rows.Next() {
		var n depNode
		if err := rows.Scan(&n.ID, &n.ContentType, &n.Value); err != nil {
			rows.Close()
			return -1, err
		}
		referrers = append(referrers, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return -1, err
	}
	rows.Close() // before reusing the connection below

	for _, n := range referrers {
		if err := updateNodeDeps(ctx, tx, n.ID, n.ContentType, n.Value); err != nil {
			return -1, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM my_config_tree_dep_meta"); err != nil {
		return -1, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO my_config_tree_dep_meta (Depth) VALUES (?)", depth); err != nil {
		return -1, err
	}
	if err := tx.Commit(); err != nil {
		return -1, err
	}
	return len(referrers), nil
}
