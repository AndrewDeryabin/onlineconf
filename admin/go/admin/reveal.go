package admin

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"

	. "github.com/onlineconf/onlineconf/admin/go/common"
)

// Reveals: the only way plaintext of an encrypted value leaves the server.
// A reveal is policy-checked against
// the caller's access, recorded in my_config_tree_reveal before any
// decryption happens, announced to the parameter's owners (botapi feed,
// legacy notifyDB), and then decrypted.

const (
	RevealView    = "view"
	RevealEdit    = "edit"
	RevealHistory = "history"
)

// ValidRevealKind reports whether a client-stated reveal kind is acceptable
// on the parameter endpoint (history is reserved to the log endpoint).
func ValidRevealKind(kind string) bool {
	return kind == RevealView || kind == RevealEdit
}

// recordReveal writes the audit row; the reveal is refused if that fails.
func recordReveal(ctx context.Context, nodeID int, path, kind string) error {
	_, err := DB.ExecContext(ctx, "INSERT INTO my_config_tree_reveal (NodeID, Path, User, Kind) VALUES (?, ?, ?, ?)",
		nodeID, path, Username(ctx), kind)
	if err != nil {
		return err
	}
	notifyReveal(ctx, nodeID, path, kind)
	return nil
}

// notifyReveal emits the legacy notifyDB message for a reveal; failures are
// logged, not returned — the audit row is the record, the message a courtesy.
func notifyReveal(ctx context.Context, nodeID int, path, kind string) {
	if notifyDB == nil {
		return
	}
	var notification string
	err := DB.QueryRowContext(ctx, "SELECT my_config_tree_notification(?)", nodeID).Scan(&notification)
	if err != nil {
		log.Ctx(ctx).Warn().Err(err).Str("path", path).Msg("failed to read the notification setting for a reveal")
		return
	}
	if notification == "none" {
		return
	}
	message := fmt.Sprintf("🔓 %s revealed %s (%s)", Username(ctx), path, kind)
	if err := insertNotification(ctx, message); err != nil {
		log.Ctx(ctx).Warn().Err(err).Str("path", path).Msg("failed to deliver the reveal notification")
	}
}

// RevealParameter replaces the parameter's masked value with its plaintext
// for the caller: policy check, audit row, decryption. A parameter without
// encrypted values is left alone (no event).
func RevealParameter(ctx context.Context, p *Parameter, kind string) error {
	if !HasEncryptedValue(p.ContentType, p.Encrypted, p.Value) {
		return nil
	}
	requireWrite, err := revealRequiresWrite(ctx)
	if err != nil {
		return err
	}
	if !canReveal(requireWrite, p.RW) {
		return ErrRevealDenied
	}
	if err := recordReveal(ctx, p.ID, p.Path, kind); err != nil {
		return err
	}
	plaintext, err := DecryptStoredValue(p.ContentType, p.Encrypted, p.storedValue)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}
	p.Value = plaintext
	p.CanReveal = true
	return nil
}

// RevealLog decrypts the encrypted versions of a history page whose node the
// caller may reveal, recording one audit row per distinct node revealed.
// Versions the policy does not admit stay masked, without error.
func RevealLog(ctx context.Context, entries []LogEntry, path string) error {
	requireWrite, err := revealRequiresWrite(ctx)
	if err != nil {
		return err
	}
	recorded := map[int]bool{}
	for i := range entries {
		l := &entries[i]
		if !HasEncryptedValue(l.ContentType, l.Encrypted, l.Value) || !canReveal(requireWrite, l.RW) {
			continue
		}
		if !recorded[l.NodeID] {
			if err := recordReveal(ctx, l.NodeID, path, RevealHistory); err != nil {
				return err
			}
			recorded[l.NodeID] = true
		}
		plaintext, err := DecryptStoredValue(l.ContentType, l.Encrypted, l.storedValue)
		if err != nil {
			return fmt.Errorf("%w: version %d: %v", ErrDecryptionFailed, l.Version, err)
		}
		l.Value = plaintext
	}
	return nil
}

// markRevealable sets can_reveal on every parameter holding an encrypted
// value, from the policy (read once) and the caller's access.
func markRevealable(ctx context.Context, params []*Parameter) error {
	requireWrite, err := revealRequiresWrite(ctx)
	if err != nil {
		return err
	}
	for _, p := range params {
		if HasEncryptedValue(p.ContentType, p.Encrypted, p.Value) {
			p.CanReveal = canReveal(requireWrite, p.RW)
		}
	}
	return nil
}

// markLogRevealable does the same for history entries.
func markLogRevealable(ctx context.Context, entries []LogEntry) error {
	requireWrite, err := revealRequiresWrite(ctx)
	if err != nil {
		return err
	}
	for i := range entries {
		l := &entries[i]
		if HasEncryptedValue(l.ContentType, l.Encrypted, l.Value) {
			l.CanReveal = canReveal(requireWrite, l.RW)
		}
	}
	return nil
}
