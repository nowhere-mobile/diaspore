package main

// Restore-completion receipts (#72, the data-loss keystone).
//
// The threat: roam-in restores a profile in THREE phases -- CE/apps (the login gate), then DE and media as
// best-effort completeness (nowhere_roamd.sh `in`). Only the CE phase gates the login; a DE or media restore
// that FAILS or half-completes still logs in "OK". A later seal (periodic sync or logoff `out`) then pushes
// that phase's partial working set OVER the good store head -- silent data loss. This was PROVEN on profile b:
// a 46%-complete media restore's seal took a 2.8 GB map from 2313 chunks -> 187, destroying it. The per-ref
// shrink-count guard in pushProfile is only a NET (it catches a DRASTIC shrink; a 60%-complete restore sails
// through, and a genuine large deletion trips it falsely).
//
// The precise fix: a per-ref RECEIPT that certifies "THIS session holds a byte-complete copy of ref R" --
// written only when a restore of R fully completes (cdcRestore returns with no chunk error) OR a seal of R
// commits successfully. pushProfile then refuses to overwrite a NON-EMPTY existing head for a ref unless a
// receipt for that ref is present. So a phase whose restore failed writes NO receipt -> its seal is refused
// -> the good head survives, PER PHASE (a failed media restore blocks only the media seal, not CE/DE).
//
// Receipts live in the RAM state tmpfs (NOWHERE_STATE), so they are per-BOOT: a fresh (amnesiac) boot starts
// with none, and roam-in clears them at the start of each login (nowhere_roamd.sh) so a receipt reflects only
// what the CURRENT session actually restored. Off-device (host CLI / unit tests) NOWHERE_STATE is unset, so
// receiptDir() == "" and the whole gate is INERT -- existing create/push/restore behavior is unchanged.
//
// The check is EXISTENCE, not head-equality: a session advances its own head on every seal, so tying the
// receipt to a specific head hash would refuse the 2nd..Nth seal (and forces awkward per-seal / capFlush
// advancement). Existence is exactly the invariant we need: "this session fully holds ref R." The stored
// content is the head hash at receipt time, kept for diagnostics only.

import (
	"os"
	"path/filepath"
)

// restoreReceiptDir is where per-ref completion receipts live, or "" when receipts are not in use (no roaming
// state dir -> host CLI / tests -> the seal gate is disabled and behavior is unchanged).
func restoreReceiptDir() string {
	st := os.Getenv("NOWHERE_STATE")
	if st == "" {
		return ""
	}
	return filepath.Join(st, "restore-receipts")
}

// receiptFile maps a store ref (a hex sha256, already filename-safe) to its receipt path, or "" when disabled.
func receiptFile(ref string) string {
	d := restoreReceiptDir()
	if d == "" || ref == "" {
		return ""
	}
	return filepath.Join(d, ref)
}

// writeRestoreReceipt records that THIS session holds a byte-complete copy of ref -- via a full restore at
// login, or a successful seal. Best-effort: a receipt we fail to write just means a later seal is refused
// (the safe direction). head is stored for diagnostics only; the gate checks existence. No-op off-device.
func writeRestoreReceipt(ref, head string) {
	f := receiptFile(ref)
	if f == "" {
		return
	}
	if os.MkdirAll(filepath.Dir(f), 0o700) != nil {
		return
	}
	os.WriteFile(f, []byte(head+"\n"), 0o600)
}

// haveRestoreReceipt reports whether THIS session has a completion receipt for ref.
func haveRestoreReceipt(ref string) bool {
	f := receiptFile(ref)
	if f == "" {
		return false
	}
	_, err := os.Stat(f)
	return err == nil
}
