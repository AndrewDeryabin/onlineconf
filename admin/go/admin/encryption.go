package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/rs/zerolog/log"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// Value encryption, admin side: the feature switch and the reveal policy
// (control parameters under /onlineconf), the key registry that lets an
// instance detect a rotated keyring at startup without reading any encrypted
// value, the background sweep that re-encrypts stored data under the new
// active key, and the schema guard.

const (
	// valueEncryptionPath is the feature switch: a truthy plain text enables
	// encrypted writes. Absent means off; fresh installations are seeded with
	// "1" by schema.sql and the example dumps.
	valueEncryptionPath = "/onlineconf/value-encryption"
	// revealPolicyPath holds "rw" (default: write access needed to reveal a
	// secret) or "ro" (read access suffices).
	revealPolicyPath = valueEncryptionPath + "/reveal"
)

var (
	ErrEncryptionNotAllowed   = errors.New("Encryption is not allowed for this content type")
	ErrEncryptionDisabled     = errors.New("Value encryption is disabled on this instance, see " + valueEncryptionPath)
	ErrEncryptedFieldRequired = errors.New("The parameter is encrypted: the encrypted field must be stated explicitly")
	ErrRevealDenied           = errors.New("Not allowed to reveal this value")
	ErrDecryptionFailed       = errors.New("Failed to decrypt the value")
)

// encryptedRowsWhere matches rows holding an encrypted leaf: the node-level
// marker, or a case with an encrypted branch (its node-level marker is off;
// the storage boundary serializes branch markers compactly).
const encryptedRowsWhere = `(Encrypted OR (ContentType = 'application/x-case' AND Value LIKE '%"encrypted":true%'))`

// plainTextParameter reads a /onlineconf control parameter directly by path —
// not through the requesting user's ACL (a global fact must not depend on
// who asks) and without following symlinks — and returns its value when it
// is a plain, unencrypted text.
func plainTextParameter(ctx context.Context, path string) (string, bool, error) {
	row := DB.QueryRowContext(ctx, "SELECT ContentType, Encrypted, Value FROM my_config_tree WHERE Path = ? AND NOT Deleted", path)
	var contentType string
	var encrypted bool
	var value sql.NullString
	err := row.Scan(&contentType, &encrypted, &value)
	if err == sql.ErrNoRows {
		return "", false, nil
	} else if err != nil {
		return "", false, err
	}
	if contentType != "text/plain" || encrypted || !value.Valid {
		return "", false, nil
	}
	return value.String, true, nil
}

// featureSwitch reports whether a control parameter holds a truthy value
// (not "" and not "0"); absent means off.
func featureSwitch(ctx context.Context, path string) (bool, error) {
	value, ok, err := plainTextParameter(ctx, path)
	if err != nil || !ok {
		return false, err
	}
	return value != "" && value != "0", nil
}

// isValueEncryptionEnabled reports whether encrypted writes are accepted on
// this instance: the switch is on and a keyring is loaded.
func isValueEncryptionEnabled(ctx context.Context) (bool, error) {
	if Keys == nil {
		return false, nil
	}
	return featureSwitch(ctx, valueEncryptionPath)
}

// revealRequiresWrite reads the reveal policy: true unless the policy
// parameter says "ro".
func revealRequiresWrite(ctx context.Context) (bool, error) {
	value, ok, err := plainTextParameter(ctx, revealPolicyPath)
	if err != nil {
		return true, err
	}
	return !(ok && strings.TrimSpace(value) == "ro"), nil
}

// revealPolicyName is the policy as served by /ui-config.
func revealPolicyName(requireWrite bool) string {
	if requireWrite {
		return "rw"
	}
	return "ro"
}

// canReveal applies the policy to the caller's access to a parameter.
func canReveal(requireWrite bool, rw NullBool) bool {
	return rw.Valid && (rw.Bool || !requireWrite)
}

// --- schema guard ------------------------------------------------------------

type schemaCheck struct {
	query     string // must select one column from the schema object checked
	migration string
}

var schemaChecks = []schemaCheck{
	{"SELECT Encrypted FROM my_config_tree LIMIT 1", "update-8.sql"},
	{"SELECT Encrypted FROM my_config_tree_log LIMIT 1", "update-8.sql"},
	{"SELECT KeyID FROM my_config_encryption_key LIMIT 1", "update-8.sql"},
	{"SELECT ID FROM my_config_tree_reveal LIMIT 1", "update-8.sql"},
}

// ensureSchemaCurrent aborts startup with an actionable message when the
// database misses a column or table this version uses unconditionally — the
// upgrade mistake that otherwise surfaces as an endless stream of
// unknown-column errors on every request. Connectivity problems are
// deliberately not judged here.
func ensureSchemaCurrent() {
	for _, check := range schemaChecks {
		var dummy interface{}
		err := DB.QueryRow(check.query).Scan(&dummy)
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if isSchemaMismatch(err) {
			log.Fatal().Err(err).Msgf("the database schema is behind this version: apply admin/etc/%s (and any earlier update-*.sql not applied yet)", check.migration)
		}
		log.Warn().Err(err).Msg("schema currency check skipped")
		return
	}
}

// isSchemaMismatch reports MySQL errors 1054 (unknown column) and 1146 (no
// such table), wrapped or not.
func isSchemaMismatch(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && (mysqlErr.Number == 1054 || mysqlErr.Number == 1146)
}

// --- advisory locks -----------------------------------------------------------

// errAdvisoryLocked reports that another admin instance holds the requested
// advisory lock (e.g. during a rolling deploy); callers skip the work.
var errAdvisoryLocked = errors.New("locked by another instance")

// acquireAdvisoryLock serializes a maintenance operation across admin
// instances with a MySQL advisory lock held on a dedicated connection
// (GET_LOCK is connection-scoped and released by the server if the
// connection dies, so a crashed holder cannot leave it stuck). The name is
// scoped to the current database: advisory locks are server-wide and
// installations may share a server.
func acquireAdvisoryLock(ctx context.Context, name string, wait time.Duration) (release func(), err error) {
	conn, err := DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	var got sql.NullInt64
	row := conn.QueryRowContext(ctx, "SELECT GET_LOCK(CONCAT(DATABASE(), '/', ?), ?)", name, int(wait/time.Second))
	if err := row.Scan(&got); err != nil {
		conn.Close()
		return nil, err
	}
	if !got.Valid || got.Int64 != 1 {
		conn.Close()
		return nil, fmt.Errorf("%s: %w", name, errAdvisoryLocked)
	}
	return func() {
		// ignore errors: closing the connection releases the lock anyway
		conn.ExecContext(ctx, "DO RELEASE_LOCK(CONCAT(DATABASE(), '/', ?))", name)
		conn.Close()
	}, nil
}

// --- key registry -------------------------------------------------------------

// fatalEncryptionError is a startup condition that must stop the server
// (as opposed to a transient database error, which is retried).
type fatalEncryptionError struct{ msg string }

func (e *fatalEncryptionError) Error() string { return e.msg }

const encryptionStartupRetry = 5 * time.Second

// initializeEncryption runs the startup checks: refuse to run without
// a keyring when encrypted values exist, verify every loaded key against the
// registry, register unknown keys, and start the sweep or the consistency
// pass depending on how the keyring's active key relates to the registered
// one. Database errors are retried in the background; the server serves in
// the meantime (the resolver fails closed on its own if the keys are wrong).
func initializeEncryption() {
	ctx := log.Logger.WithContext(context.Background())
	run := func() bool {
		err := encryptionStartup(ctx)
		var fatal *fatalEncryptionError
		if errors.As(err, &fatal) {
			log.Fatal().Msg(fatal.msg)
		}
		if err != nil {
			log.Warn().Err(err).Msg("encryption startup check failed, will retry")
			return false
		}
		return true
	}
	if run() {
		return
	}
	go func() {
		for !run() {
			time.Sleep(encryptionStartupRetry)
		}
	}()
}

func encryptionStartup(ctx context.Context) error {
	if Keys == nil {
		var n int
		if err := DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM my_config_tree WHERE "+encryptedRowsWhere).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return &fatalEncryptionError{"encrypted parameters exist but encryption.key_file is not configured"}
		}
		return nil
	}

	for _, key := range Keys.All() {
		if err := registerKey(ctx, key); err != nil {
			return err
		}
	}

	activeID, activeSeq, hasActive, err := registeredActiveKey(ctx)
	if err != nil {
		return err
	}
	mine := Keys.Active()
	mySeq, err := keySeq(ctx, mine.ID)
	if err != nil {
		return err
	}
	switch {
	case !hasActive:
		if err := setRegisteredActive(ctx, mine.ID); err != nil {
			return err
		}
		log.Info().Str("key", mine.ID).Msg("encryption key registered as active")
	case activeID == mine.ID:
		go runEncryptionSweep(false, mine.ID)
	case mySeq > activeSeq:
		log.Info().Str("from", activeID).Str("to", mine.ID).Msg("encryption key rotation detected, re-encrypting stored values in the background")
		go runEncryptionSweep(true, mine.ID)
	default:
		log.Error().Str("keyring_active", mine.ID).Str("registered_active", activeID).
			Msg("the keyring's active key is older than the registered active key: this instance has a stale key file and will keep writing under the old key until restarted with the current one")
	}
	return nil
}

// registerKey verifies a key against its registry row, or registers it. A
// key whose material differs from the registered one under the same id is
// fatal: it is the classic operator error, and it would surface only as
// unexplained decryption failures otherwise.
func registerKey(ctx context.Context, key *Key) error {
	verifier, err := selectVerifier(ctx, key.ID)
	if err == sql.ErrNoRows {
		v, err := key.Verifier()
		if err != nil {
			return err
		}
		if _, err := DB.ExecContext(ctx, "INSERT IGNORE INTO my_config_encryption_key (KeyID, Verifier) VALUES (?, ?)", key.ID, v); err != nil {
			return err
		}
		// re-read: another instance may have registered the same id first
		verifier, err = selectVerifier(ctx, key.ID)
		if err != nil {
			return err
		}
		log.Info().Str("key", key.ID).Msg("encryption key registered")
	} else if err != nil {
		return err
	}
	if !key.CheckVerifier(verifier) {
		return &fatalEncryptionError{fmt.Sprintf("encryption key %s in the key file differs from the key registered under that id in the database", key.ID)}
	}
	return nil
}

func selectVerifier(ctx context.Context, keyID string) ([]byte, error) {
	var verifier []byte
	err := DB.QueryRowContext(ctx, "SELECT Verifier FROM my_config_encryption_key WHERE KeyID = ?", keyID).Scan(&verifier)
	return verifier, err
}

func keySeq(ctx context.Context, keyID string) (int64, error) {
	var seq int64
	err := DB.QueryRowContext(ctx, "SELECT Seq FROM my_config_encryption_key WHERE KeyID = ?", keyID).Scan(&seq)
	return seq, err
}

func registeredActiveKey(ctx context.Context) (id string, seq int64, ok bool, err error) {
	err = DB.QueryRowContext(ctx, "SELECT KeyID, Seq FROM my_config_encryption_key WHERE Active").Scan(&id, &seq)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	return id, seq, err == nil, err
}

func setRegisteredActive(ctx context.Context, keyID string) error {
	_, err := DB.ExecContext(ctx, "UPDATE my_config_encryption_key SET Active = (KeyID = ?)", keyID)
	return err
}

// --- sweep --------------------------------------------------------------------

// sweepStatus is the per-instance state of the last sweep, served by the
// status endpoint.
type sweepStatus struct {
	State       string `json:"state"` // idle, running, done, failed
	Target      string `json:"target,omitempty"`
	Full        bool   `json:"full"`
	Scanned     int    `json:"scanned"`
	Reencrypted int    `json:"reencrypted"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

var (
	sweepMu    sync.Mutex
	sweepState = sweepStatus{State: "idle"}
)

const (
	sweepBatch    = 500
	sweepLockWait = 10 * time.Second
)

// runEncryptionSweep re-encrypts stored values under the active key. A full
// sweep (rotation) walks the whole tree and log and flips the registry's
// Active on completion; the consistency pass (same active key) walks the tree
// only and follows up the log rows of the nodes it had to fix — what a peer
// writing under a stale key file leaves behind.
func runEncryptionSweep(full bool, target string) {
	ctx := log.Logger.WithContext(context.Background())
	release, err := acquireAdvisoryLock(ctx, "encryption-sweep", sweepLockWait)
	if errors.Is(err, errAdvisoryLocked) {
		log.Info().Msg("another instance is re-encrypting stored values; skipping")
		return
	} else if err != nil {
		log.Error().Err(err).Msg("failed to acquire the encryption sweep lock")
		return
	}
	defer release()

	sweepMu.Lock()
	sweepState = sweepStatus{State: "running", Target: target, Full: full, StartedAt: time.Now().Format(time.RFC3339)}
	sweepMu.Unlock()

	scanned, reencrypted, err := encryptionSweep(ctx, full)

	sweepMu.Lock()
	sweepState.Scanned, sweepState.Reencrypted = scanned, reencrypted
	sweepState.FinishedAt = time.Now().Format(time.RFC3339)
	if err != nil {
		sweepState.State, sweepState.Error = "failed", err.Error()
	} else {
		sweepState.State = "done"
	}
	sweepMu.Unlock()

	if err != nil {
		log.Error().Err(err).Bool("full", full).Msg("re-encryption of stored values failed")
		return
	}
	if full {
		if err := setRegisteredActive(ctx, target); err != nil {
			log.Error().Err(err).Msg("failed to record the new active encryption key")
			return
		}
	}
	event := log.Info().Bool("full", full).Int("scanned", scanned).Int("reencrypted", reencrypted)
	if reencrypted > 0 && !full {
		event = log.Warn().Bool("full", full).Int("scanned", scanned).Int("reencrypted", reencrypted)
	}
	event.Str("key", target).Msg("re-encryption of stored values finished")
}

func encryptionSweep(ctx context.Context, full bool) (scanned, reencrypted int, err error) {
	nodeIDs, err := reencryptTable(ctx, "my_config_tree", "", &scanned, &reencrypted)
	if err != nil {
		return
	}
	if full {
		_, err = reencryptTable(ctx, "my_config_tree_log", "", &scanned, &reencrypted)
		return
	}
	if len(nodeIDs) == 0 {
		return
	}
	// the consistency pass follows up only the fixed nodes' history
	marks := make([]string, len(nodeIDs))
	for i := range marks {
		marks[i] = fmt.Sprint(nodeIDs[i])
	}
	_, err = reencryptTable(ctx, "my_config_tree_log", "NodeID IN ("+strings.Join(marks, ", ")+")", &scanned, &reencrypted)
	return
}

type sweepRow struct {
	id          int64
	nodeID      int64
	contentType string
	encrypted   bool
	value       NullString
}

// reencryptTable walks a table in ID batches and rewrites every value that
// holds an envelope under a non-active key, with a compare-and-swap so a
// concurrent write (already under the active key) always wins. It returns
// the node IDs of the rows it rewrote.
func reencryptTable(ctx context.Context, table, extraWhere string, scanned, reencrypted *int) ([]int64, error) {
	nodeColumn := "ID"
	if table == "my_config_tree_log" {
		nodeColumn = "NodeID"
	}
	where := encryptedRowsWhere
	if extraWhere != "" {
		where += " AND " + extraWhere
	}
	var nodeIDs []int64
	seen := map[int64]bool{}
	lastID := int64(0)
	for {
		rows, err := DB.QueryContext(ctx, "SELECT ID, "+nodeColumn+", ContentType, Encrypted, Value FROM "+table+
			" WHERE ID > ? AND "+where+" ORDER BY ID LIMIT ?", lastID, sweepBatch)
		if err != nil {
			return nodeIDs, err
		}
		batch := make([]sweepRow, 0, sweepBatch)
		for rows.Next() {
			var r sweepRow
			if err := rows.Scan(&r.id, &r.nodeID, &r.contentType, &r.encrypted, &r.value); err != nil {
				rows.Close()
				return nodeIDs, err
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nodeIDs, err
		}
		rows.Close()
		if len(batch) == 0 {
			return nodeIDs, nil
		}
		for _, r := range batch {
			lastID = r.id
			*scanned++
			newValue, changed, err := ReencryptStoredValue(r.contentType, r.encrypted, r.value)
			if err != nil {
				return nodeIDs, fmt.Errorf("%s row %d: %w", table, r.id, err)
			}
			if !changed {
				continue
			}
			res, err := DB.ExecContext(ctx, "UPDATE "+table+" SET Value = ? WHERE ID = ? AND Value = ?", newValue, r.id, r.value)
			if err != nil {
				return nodeIDs, err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				*reencrypted++
				if !seen[r.nodeID] {
					seen[r.nodeID] = true
					nodeIDs = append(nodeIDs, r.nodeID)
				}
			}
		}
	}
}

// --- status -------------------------------------------------------------------

type keyStatus struct {
	ID         string `json:"id"`
	Active     bool   `json:"active"`     // first line of the keyring
	Registered bool   `json:"registered"` // present in the registry
	Verified   bool   `json:"verified"`   // material matches the registry
	Recorded   bool   `json:"recorded"`   // the registry's Active mark
	Seq        int64  `json:"seq,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
}

type EncryptionStatus struct {
	Configured bool        `json:"configured"`
	Keys       []keyStatus `json:"keys"`
	Sweep      sweepStatus `json:"sweep"`
}

// SelectEncryptionStatus answers the operator's question "may the old key go
// now?": the keyring's keys with their registry state and the sweep state.
func SelectEncryptionStatus(ctx context.Context) (*EncryptionStatus, error) {
	status := &EncryptionStatus{Configured: Keys != nil, Keys: []keyStatus{}}
	rows, err := DB.QueryContext(ctx, "SELECT KeyID, Seq, Verifier, Active, CreatedAt FROM my_config_encryption_key ORDER BY Seq")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	registered := map[string]keyStatus{}
	var order []string
	for rows.Next() {
		var ks keyStatus
		var verifier []byte
		if err := rows.Scan(&ks.ID, &ks.Seq, &verifier, &ks.Recorded, &ks.CreatedAt); err != nil {
			return nil, err
		}
		ks.Registered = true
		if Keys != nil {
			if key, ok := Keys.Key(ks.ID); ok {
				ks.Verified = key.CheckVerifier(verifier)
			}
		}
		registered[ks.ID] = ks
		order = append(order, ks.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if Keys != nil {
		for i, key := range Keys.All() {
			ks, ok := registered[key.ID]
			if !ok {
				ks = keyStatus{ID: key.ID}
			}
			ks.Active = i == 0
			registered[key.ID] = ks
			if !ok {
				order = append(order, key.ID)
			}
		}
	}
	for _, id := range order {
		status.Keys = append(status.Keys, registered[id])
	}
	sweepMu.Lock()
	status.Sweep = sweepState
	sweepMu.Unlock()
	return status, nil
}
