// Nowhere on-device roaming agent (Phase 2: P2.2 on-device, P2.3 blind-login, P2.5 lazy).
//
// Static Go binary that runs ON the device over its own network: HTTP to the content-addressed
// store + AES-256-GCM + tar. (Also runs on the host for key/manifest model tests; logic is identical.)
//
// P2.3 blind-login keys:  ref = sha256("nowhere-ref:"+name) ; key = Argon2id(pass, sha256("nowhere-salt:"+name)).
// P2.5 lazy restore:      the profile head points at a MANIFEST of items (each its own sealed,
//   content-addressed blob) with priorities. restore-set <maxPrio> restores only items at/under
//   that priority, so the WORKING SET comes back first (device usable) and bulk is deferred.
// P3.1 login-daemon:      the root side of the blind-login chooser. init hands us an AF_UNIX socket;
//   the chooser app connects and sends "<name>\n<pass>\n"; we restore that profile's working set into
//   /data/nowhere/state IN-PROCESS (passphrase only ever in memory, never on disk or in argv).
//
// Stores (selected by the <store> arg):
//   - an HTTP store (Phase 0 store_server.py):  POST /blob -> hash ; GET /blob/<hash> ; GET|PUT /ref/<name>
//   - "s3"  -> any S3-compatible store (Filebase/Sia, Cloudflare R2, Backblaze B2, MinIO). P4.2b.
//             config via env: S3_ENDPOINT S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET [S3_REGION].
//             blobs -> object blob/<hash> (immutable) ; head -> object ref/<name> (mutable).
// Build:  CGO_ENABLED=0 go build -o nowhere_agent .
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/argon2"
	"golang.org/x/sys/unix"
)

type Item struct {
	Name string `json:"name"`
	Prio int    `json:"prio"`
	Ref  string `json:"ref"`
	Size int    `json:"size"`
}
type Manifest struct {
	Items []Item `json:"items"`
}

// profileRef is the store key for an identity's mutable head, bound to BOTH name and passphrase: a
// wrong passphrase derives a different (unguessable) ref that resolves to nothing -> blind login, and
// enrollment can refuse an existing (name,pass) without revealing which *names* exist.
// profileRefLegacy is the ORIGINAL (pre-#80) head ref: a bare sha256 -> a FAST existence oracle (a store dump
// lets someone grind passphrase guesses for a target name at billions/sec). Kept only for fallback + migration.
func profileRefLegacy(name, passphrase string) string {
	h := sha256.Sum256([]byte("nowhere-ref:" + name + "\x00" + passphrase))
	return hex.EncodeToString(h[:])
}

// profileRefV2 (#80) derives the head ref from the Argon2 key K instead of a bare hash, so a store dump costs
// the FULL KDF per guess (no cheap oracle). deriveKey is memoized, so K is reused from the KEK/DK -> zero added
// login latency. profileRef is retired; callers use headKey/putHead (which pick v2 or legacy per what exists).
func profileRefV2(name, passphrase string) string {
	mac := hmac.New(sha256.New, deriveKey(name, passphrase))
	mac.Write([]byte("nowhere-ref-v2"))
	return hex.EncodeToString(mac.Sum(nil))
}

// deriveKey memo (#80): profileRefV2 + the KEK + the DK all need K = Argon2id(pass, salt(name)) -- 64 MB and
// slow. Compute it ONCE per (name,pass) per process and reuse, so v2 refs add no Argon2. K already lives in RAM
// during the op; the cache doesn't widen its exposure. BOUNDED (a long-lived daemon serves many profiles), and
// safe to cache: K is a pure function of (name,pass), unlike headKey which depends on mutable store state.
var deriveKeyMu sync.Mutex
var deriveKeyCache = map[string][]byte{}
var deriveKeyMisses int // test hook: counts ACTUAL Argon2 computations (cache misses)

func deriveKey(name, passphrase string) []byte {
	ck := name + "\x00" + passphrase
	deriveKeyMu.Lock()
	if v, ok := deriveKeyCache[ck]; ok {
		deriveKeyMu.Unlock()
		return v
	}
	deriveKeyMu.Unlock()
	salt := sha256.Sum256([]byte("nowhere-salt:" + name))
	k := argon2.IDKey([]byte(passphrase), salt[:], 1, 64*1024, 4, 32)
	deriveKeyMu.Lock()
	if len(deriveKeyCache) >= 16 { // cap the daemon's live-key set; CLI ops exit long before this matters
		deriveKeyCache = map[string][]byte{}
	}
	deriveKeyCache[ck] = k
	deriveKeyMisses++
	deriveKeyMu.Unlock()
	return k
}

// headKey resolves the ref a profile's head CURRENTLY lives at: v2 if present (migrated or new), else the
// legacy sha256 ref if THAT is present (un-migrated), else v2 (a brand-new profile -> writes land at v2). Reads
// (getRef/refExists/delRef) and bare-ref uses (lease/footprint key/receipt/anchor/rollback) go through this.
// NOT memoized -- the head can migrate in a different process (the seal worker), so a cached ref could go stale
// and cause a false blank login; the refExists checks here are few and cheap (deriveKey's Argon2 is memoized).
func headKey(base, name, passphrase string) string {
	if v2 := profileRefV2(name, passphrase); refExists(base, v2) {
		return v2
	}
	if lg := profileRefLegacy(name, passphrase); refExists(base, lg) {
		return lg
	}
	return profileRefV2(name, passphrase)
}

// putHead writes a profile's head at the v2 ref (migrating a legacy profile on its first write), then TOMBSTONES
// the legacy ref so the sha256 oracle no longer serves data and a stale fallback can't resurface the old head.
func putHead(base, name, passphrase, val string) {
	v2 := profileRefV2(name, passphrase)
	putRef(base, v2, val)
	if lg := profileRefLegacy(name, passphrase); lg != v2 && refExists(base, lg) {
		delRef(base, lg) // tombstone (empty-but-exists); reads try v2 first, so it's never consulted post-migration
	}
}

// ---- Recovery / passphrase-reset: key-wrapping vault (keyslots). See docs/recovery.md. ----
// A random Data Key (DK) encrypts the profile's data; DK is WRAPPED under a passphrase-KEK and a
// recovery-KEK in a plaintext "vault" header at the ref. So a forgotten passphrase is recoverable via a
// one-time 12-word code, and changing the passphrase just re-wraps DK (no data re-encryption). Blind login
// is preserved -- the vault is reachable only via an unguessable ref (one from the pass, one from the code).

var vaultMagic = []byte("DSPRVLT1")

type keyslot struct {
	Kind    string `json:"kind"`    // "pass" | "recovery"
	Wrapped string `json:"wrapped"` // base64(nonce|ciphertext|tag) of DK under this slot's KEK
}

// headSnap is one retained prior head (#58): enough to roll back to it -- the sealed CDC manifest hash and
// its version/time. The manifest's chunks stay alive in the store because the footprint leases the last-K
// snapshots' chunks (billing.go); dedup makes that the delta cost only.
type headSnap struct {
	Version  uint64 `json:"version"`
	Manifest string `json:"manifest"`
	Time     int64  `json:"time,omitempty"`
	Kind     string `json:"kind,omitempty"` // #58: "manual" (a Back up now save-point -- pinned) or "" (automatic)
}

type vault struct {
	V           int        `json:"v"`
	Slots       []keyslot  `json:"slots"`
	Manifest    string     `json:"manifest"`     // content-hash of the DK-sealed CDC chunk-manifest (the data head)
	RecoveryRef string     `json:"recovery_ref"` // f(name,entropy) -- stored so push keeps it in sync (a hash; leaks nothing)
	Version     uint64     `json:"version"`      // monotonic head version: bumped on every push/rotate -> rollback signal
	LastSeal    int64      `json:"last_seal,omitempty"` // unix time of the most recent push -> "last active" on the welcome-back (DIA-20260625-05); omitempty keeps old signatures verifying
	History     []headSnap `json:"history,omitempty"`   // #58: retained prior heads (newest-first) for rollback recovery; omitempty keeps pre-#58 signatures verifying
	HeadKind    string     `json:"head_kind,omitempty"` // #58: kind of the LIVE head ("manual"/"") -> the NEXT seal snapshots it with this kind
	Sig         string     `json:"sig"`          // base64 HMAC over the header (Sig cleared), keyed by the DK -> tamper-evidence
	Hardened    bool       `json:"hardened,omitempty"` // Endospore E.3b: pass-only slot dropped -> open only via `se` (SE device) or `recovery`
}

// sealKind (#58): a "manual" seal is the explicit "Back up now" -- the daemon sets NOWHERE_SEAL_KIND=manual
// on the roam.req and the worker exports it, so the seal here reads it. Everything else (periodic sync,
// logoff) is automatic ("").
func sealKind() string {
	if os.Getenv("NOWHERE_SEAL_KIND") == "manual" {
		return "manual"
	}
	return ""
}

func snapEnv(name string, def int) int {
	if s := os.Getenv(name); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	return def
}

// #58 TWO-CLASS snapshot retention, because the on-device test showed the ~1/min periodic sync churns the
// history every minute (background app writes change a few chunks even with no user action):
//   MANUAL saves ("Back up now") are deliberate, meaningful save-points -> keep the newest snapManualMax(),
//   PINNED (an automatic sync can NEVER evict them).
//   AUTOMATIC saves (periodic) are noisy churn states -> keep a TIME-SPACED subset (the newest few + one/hour
//   for a few hours + one/day for a few days), capped, so they span days not minutes and don't churn out from
//   under the user (which caused the "no retained snapshot" race). Bounded total = manual cap + auto cap.
func snapManualMax() int  { return snapEnv("NOWHERE_SNAP_MANUAL", 4) } // 4 manual + 5 auto = 9 max -> fits one screen
func snapAutoMax() int    { return snapEnv("NOWHERE_SNAP_AUTO", 5) }
func snapAutoRecent() int { return snapEnv("NOWHERE_SNAP_AUTO_RECENT", 2) }
func snapAutoHours() int  { return snapEnv("NOWHERE_SNAP_AUTO_HOURS", 6) }
func snapAutoDays() int   { return snapEnv("NOWHERE_SNAP_AUTO_DAYS", 5) }

// pushSnapshot records the OUTGOING head (with ITS kind, carried in the vault's HeadKind) as a rollback
// snapshot before the live head is repointed, then prunes History to the two-class set. No-op when the data
// didn't change (a no-change re-seal bumps version but adds no new snapshot).
func pushSnapshot(v *vault, newManifest, outgoingKind string) {
	if v.Manifest == "" || v.Manifest == newManifest {
		return
	}
	snap := headSnap{Version: v.Version, Manifest: v.Manifest, Time: v.LastSeal, Kind: outgoingKind}
	v.History = pruneSnapshots(append([]headSnap{snap}, v.History...), time.Now().Unix())
}

// pruneSnapshots keeps the two-class retention set from a newest-first History, merged newest-first.
func pruneSnapshots(hist []headSnap, now int64) []headSnap {
	var manual, auto []headSnap
	for _, s := range hist { // partition; both stay newest-first
		if s.Kind == "manual" {
			manual = append(manual, s)
		} else {
			auto = append(auto, s)
		}
	}
	if m := snapManualMax(); m >= 0 && len(manual) > m { // PIN the newest manual saves
		manual = manual[:m]
	}
	return mergeByVersionDesc(manual, spaceAuto(auto, now)) // time-space the automatic saves, then merge
}

// spaceAuto keeps a time-distributed subset of automatic snapshots (newest-first): the newest few, then one
// per hour for a few hours, then one per day for a few days, up to snapAutoMax().
func spaceAuto(auto []headSnap, now int64) []headSnap {
	max, recent := snapAutoMax(), snapAutoRecent()
	hourWin, dayWin := int64(snapAutoHours())*3600, int64(snapAutoDays())*86400
	keep := make([]headSnap, 0, max)
	seen := map[int64]bool{}
	for i, s := range auto {
		if len(keep) >= max {
			break
		}
		if i < recent { // the newest few autos: always keep
			keep = append(keep, s)
			continue
		}
		age := now - s.Time
		var bucket int64
		switch {
		case age <= hourWin:
			bucket = s.Time / 3600 // hourly
		case age <= dayWin:
			bucket = 1_000_000_000 + s.Time/86400 // daily (offset so it can't collide with an hour bucket)
		default:
			continue // older than the daily window -> drop
		}
		if !seen[bucket] {
			keep = append(keep, s)
			seen[bucket] = true
		}
	}
	return keep
}

// mergeByVersionDesc merges two newest-first (Version-descending) snapshot lists into one, newest-first.
func mergeByVersionDesc(a, b []headSnap) []headSnap {
	out := make([]headSnap, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Version >= b[j].Version {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return append(append(out, a[i:]...), b[j:]...)
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		check(err)
	}
	return b
}

// wrap/unwrap protect a key under a KEK with AES-256-GCM + a RANDOM nonce (NOT seal()'s convergent nonce --
// a wrapped key is secret, and we want fresh ciphertext on each wrap).
func wrap(kek, plaintext []byte) string {
	block, _ := aes.NewCipher(kek)
	gcm, err := cipher.NewGCM(block)
	check(err)
	nonce := randBytes(gcm.NonceSize())
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil))
}

func unwrap(kek []byte, wrapped string) ([]byte, bool) {
	ct, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil {
		return nil, false
	}
	block, e := aes.NewCipher(kek)
	if e != nil {
		return nil, false
	}
	gcm, e := cipher.NewGCM(block)
	if e != nil {
		return nil, false
	}
	ns := gcm.NonceSize()
	if len(ct) < ns {
		return nil, false
	}
	pt, e := gcm.Open(nil, ct[:ns], ct[ns:], nil)
	if e != nil {
		return nil, false
	}
	return pt, true
}

// KEKs: the passphrase KEK is deriveKey; the recovery KEK is Argon2id over the 128-bit recovery entropy
// (what the 12 words encode) with a distinct per-name salt.
func kekRecovery(name string, entropy []byte) []byte {
	salt := sha256.Sum256([]byte("nowhere-recovery-salt:" + name))
	return argon2.IDKey(entropy, salt[:], 1, 64*1024, 4, 32)
}

// kekSE is the secure-element-bound passphrase KEK for a HARDENED (Endospore) identity. se_secret is a
// 32-byte random value sealed in the device's Titan M2 (StrongBox) and released only after a Weaver-throttled
// passphrase check (chooser-side). Mixing it into the salt means the `se` keyslot opens ONLY with
// (passphrase + that device's se_secret): an attacker who captures the store ciphertext cannot brute-force
// the passphrase offline (no se_secret), and the SE rate-limits on-device guesses. Argon2 (not a bare HKDF)
// keeps it slow even if se_secret ever leaks. The SE device delivers se_secret via NOWHERE_SE_SECRET (hex);
// it is never persisted by the agent. See docs/se-binding.md (E.3b).
func kekSE(name, pass string, seSecret []byte) []byte {
	salt := sha256.Sum256(append([]byte("nowhere-se-salt:"+name+"\x00"), seSecret...))
	return argon2.IDKey([]byte(pass), salt[:], 1, 64*1024, 4, 32)
}

// seDK tries to open DK from a vault's secure-element slot using the se_secret this device supplied via
// NOWHERE_SE_SECRET (hex). Returns nil when no se_secret is set or no `se` slot opens -- so callers fall
// back to the pass/recovery paths transparently. (unwrapDK iterates ALL `se` slots, so multi-device works.)
func seDK(v *vault, name, pass string) []byte {
	ss := os.Getenv("NOWHERE_SE_SECRET")
	if ss == "" {
		return nil
	}
	sb, err := hex.DecodeString(strings.TrimSpace(ss))
	if err != nil || len(sb) == 0 {
		return nil
	}
	return unwrapDK(v, "se", kekSE(name, pass, sb))
}

// recoveryRef is the unguessable store location for the vault via the recovery code -- the parallel of
// profileRef for the recovery path. A hash, so storing it in the vault leaks nothing.
func recoveryRef(name string, entropy []byte) string {
	h := sha256.Sum256([]byte("nowhere-recovery-ref:" + name + "\x00" + hex.EncodeToString(entropy)))
	return hex.EncodeToString(h[:])
}

// ---- Discovery / bootstrap (Tier 2 target): name+passphrase -> the device's STORE config, so a profile
// re-materializes on any Nowhere device without re-entering store creds. At enrollment we seal the
// store-config under a (name,pass)-derived key and PUT it at an unguessable bootstrapRef on a baked
// DISCOVERY endpoint; a fresh device derives the same ref, GETs the ciphertext, unseals, and configures
// itself. The discovery endpoint only ever sees unguessable refs -> ciphertext (zero-knowledge), exactly
// like the data store. bootstrapRef is distinct from profileRef (different hash prefix), so discovery and
// data may even share one store without colliding. See docs/enrollment.md §3. ----

func bootstrapRef(name, passphrase string) string {
	h := sha256.Sum256([]byte("nowhere-bootstrap-ref:" + name + "\x00" + passphrase))
	return hex.EncodeToString(h[:])
}

// bootstrapKey wraps the sealed store-config -- a key separate from the data/pass key (its own salt).
func bootstrapKey(name, passphrase string) []byte {
	salt := sha256.Sum256([]byte("nowhere-bootstrap-salt:" + name))
	return argon2.IDKey([]byte(passphrase), salt[:], 1, 64*1024, 4, 32)
}

// publishDiscovery seals the store-config (the 5 S3_* conf lines, plus the billing GATEWAY_URL when set)
// under bootstrapKey(name,pass) and PUTs it to the discovery store at bootstrapRef -- so a future device
// can discover it from name+pass alone. The gateway URL rides along so a user's billing endpoint roams
// with their store; an empty gw is simply omitted (older readers ignore the unknown key either way).
func publishDiscovery(discoStore, name, pass, ep, rg, bk, ak, sk, gw string) {
	cfg := fmt.Sprintf("S3_ENDPOINT=%s\nS3_REGION=%s\nS3_BUCKET=%s\nS3_ACCESS_KEY=%s\nS3_SECRET_KEY=%s\n", ep, rg, bk, ak, sk)
	if gw != "" {
		cfg += "GATEWAY_URL=" + gw + "\n"
	}
	putRef(discoStore, bootstrapRef(name, pass), wrap(bootstrapKey(name, pass), []byte(cfg)))
}

// discoverConfig fetches + unseals the store-config for (name,pass) from the discovery store. Returns the
// conf text + true on success; "", false when there's nothing published (or it won't unseal). Blind: a
// wrong name/pass derives a different bootstrapRef -> a GET miss, indistinguishable from "not enrolled".
func discoverConfig(discoStore, name, pass string) (string, bool) {
	w := getRef(discoStore, bootstrapRef(name, pass))
	if w == "" {
		return "", false
	}
	pt, ok := unwrap(bootstrapKey(name, pass), w)
	if !ok {
		return "", false
	}
	return string(pt), true
}

func parseVault(blob []byte) (*vault, bool) {
	if !bytes.HasPrefix(blob, vaultMagic) {
		return nil, false
	}
	var v vault
	if json.Unmarshal(blob[len(vaultMagic):], &v) != nil || v.V == 0 || len(v.Slots) == 0 {
		return nil, false
	}
	return &v, true
}

func serializeVault(v *vault) []byte {
	j, _ := json.Marshal(v)
	return append(append([]byte{}, vaultMagic...), j...)
}

// unwrapDK returns DK from the first slot of the given kind that the KEK opens (nil if none).
func unwrapDK(v *vault, kind string, kek []byte) []byte {
	for _, s := range v.Slots {
		if s.Kind == kind {
			if dk, ok := unwrap(kek, s.Wrapped); ok {
				return dk
			}
		}
	}
	return nil
}

// ---- Head integrity: sign the vault header (tamper-evidence) + a monotonic version (rollback signal). ----
// The signature is an HMAC over the canonical header (Sig cleared) keyed by a DK-derived key -- so the
// store, even with its credentials, cannot forge or alter a head without the passphrase (which yields DK).

func headSigKey(dk []byte) []byte {
	h := hmac.New(sha256.New, dk)
	h.Write([]byte("nowhere-head-sig"))
	return h.Sum(nil)
}

func signVault(v *vault, dk []byte) {
	v.Sig = ""
	mac := hmac.New(sha256.New, headSigKey(dk))
	mac.Write(serializeVault(v)) // canonical: the header with Sig=""
	v.Sig = base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func verifyVault(v *vault, dk []byte) bool {
	want := v.Sig
	if want == "" {
		return false
	}
	v.Sig = ""
	mac := hmac.New(sha256.New, headSigKey(dk))
	mac.Write(serializeVault(v))
	v.Sig = want
	return hmac.Equal([]byte(base64.StdEncoding.EncodeToString(mac.Sum(nil))), []byte(want))
}

// ---- Rollback anchor (OPT-IN via NOWHERE_ROLLBACK_ANCHOR=1). Persists the highest head version seen per
// profileRef in NOWHERE_ROLLBACK_DIR (default /data/nowhere/rollback, which survives the power-off wipe)
// and rejects a head whose version is behind it -- a replay of an old, validly-signed head. OFF by default:
// the anchor records (as unguessable hashes) that profiles logged in here, trading the device's plausible
// deniability -- so it's an explicit choice for trusted / non-duress deployments. ----

func rollbackEnforced() bool { return os.Getenv("NOWHERE_ROLLBACK_ANCHOR") == "1" }

func anchorDir() string {
	if d := os.Getenv("NOWHERE_ROLLBACK_DIR"); d != "" {
		return d
	}
	return "/data/nowhere/rollback"
}

func readAnchor(ref string) uint64 {
	b, err := os.ReadFile(filepath.Join(anchorDir(), ref))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// bumpAnchor records ver if it's higher than what's stored (monotonic). No-op unless enforcement is on.
func bumpAnchor(ref string, ver uint64) {
	if !rollbackEnforced() || ver <= readAnchor(ref) {
		return
	}
	os.MkdirAll(anchorDir(), 0o700)
	os.WriteFile(filepath.Join(anchorDir(), ref), []byte(strconv.FormatUint(ver, 10)), 0o600)
}

// checkRollback fails the restore if ver is behind the anchored version (an old-head replay), then advances
// the anchor. No-op unless enforcement is on.
func checkRollback(ref string, ver uint64) {
	if !rollbackEnforced() {
		return
	}
	if a := readAnchor(ref); ver < a {
		fail(fmt.Sprintf("rollback detected for head: version %d < anchored %d", ver, a))
	}
	bumpAnchor(ref, ver)
}

// ---- Enrollment rate-limit (Tier 3). The gate's CREATE path is open -- anyone at the device can mint new
// (name,pass) profiles, so a script could flood the store with junk heads/blobs. A per-device token bucket
// throttles CREATE: capacity NOWHERE_ENROLL_MAX (default 5), fully refilled over NOWHERE_ENROLL_WINDOW
// seconds (default 3600). A fresh device starts full (a burst of MAX covers normal first-run setup), then
// allows ~WINDOW/MAX between creates. State lives at /data/nowhere/enroll, which -- like the rollback anchor
// and the conf -- survives the power-off wipe, so the limit can't be reset by power-cycling. This is a LOCAL
// throttle (one device); the cross-device / store-side enrollment gate is a Phase-2 broker. Set
// NOWHERE_ENROLL_MAX=0 to disable (turnkey / trusted deployments). ----

type enrollBucket struct {
	Tokens float64 `json:"tokens"`
	TS     int64   `json:"ts"`
}

var enrollMu sync.Mutex

func enrollStatePath() string {
	if p := os.Getenv("NOWHERE_ENROLL_STATE"); p != "" {
		return p
	}
	return "/data/nowhere/enroll"
}

func enrollFloat(env string, def float64) float64 {
	if v := os.Getenv(env); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

// enrollAllow consumes one enrollment token. Returns (true,0) if a CREATE may proceed, or (false,
// retryAfterSeconds) when the bucket is empty. No-op (always allows) when NOWHERE_ENROLL_MAX<=0.
func enrollAllow() (bool, int) {
	max := enrollFloat("NOWHERE_ENROLL_MAX", 5)
	if max <= 0 {
		return true, 0 // limiter disabled
	}
	win := enrollFloat("NOWHERE_ENROLL_WINDOW", 3600)
	if win <= 0 {
		win = 3600
	}
	rate := max / win // tokens accrued per second
	enrollMu.Lock()
	defer enrollMu.Unlock()
	now := time.Now().Unix()
	b := enrollBucket{Tokens: max, TS: now} // unseen device: start with a full bucket
	if raw, err := os.ReadFile(enrollStatePath()); err == nil {
		var prev enrollBucket
		if json.Unmarshal(raw, &prev) == nil && prev.TS > 0 {
			b = prev
			if elapsed := float64(now - b.TS); elapsed > 0 {
				if b.Tokens += elapsed * rate; b.Tokens > max {
					b.Tokens = max
				}
			}
			b.TS = now
		}
	}
	if b.Tokens >= 1 {
		b.Tokens--
		saveEnroll(b)
		return true, 0
	}
	retry := int((1 - b.Tokens) / rate) // seconds until one whole token accrues
	if float64(retry)*rate < (1 - b.Tokens) {
		retry++ // round up
	}
	if retry < 1 {
		retry = 1
	}
	saveEnroll(b) // persist the advanced timestamp/accrual even on denial
	return false, retry
}

func saveEnroll(b enrollBucket) {
	os.MkdirAll(filepath.Dir(enrollStatePath()), 0o700)
	if raw, err := json.Marshal(b); err == nil {
		os.WriteFile(enrollStatePath(), raw, 0o600)
	}
}

// buildVault wraps DK under the pass + recovery KEKs and records the manifest + recovery ref.
func buildVault(dk []byte, name, pass string, entropy []byte, manifestHash string) *vault {
	return &vault{
		V: 1,
		Slots: []keyslot{
			{Kind: "pass", Wrapped: wrap(deriveKey(name, pass), dk)},
			{Kind: "recovery", Wrapped: wrap(kekRecovery(name, entropy), dk)},
		},
		Manifest:    manifestHash,
		RecoveryRef: recoveryRef(name, entropy),
	}
}

// resolveKey returns the data key + the manifest-blob bytes for (name, pass), transparently handling BOTH
// the new vault format and the legacy bare-manifest head. ok=false => the ref didn't resolve (blank login).
func resolveKey(base, name, pass string) (key, manifestBlob []byte, ok bool) {
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return nil, nil, false
	}
	blob := getBlob(base, head)
	if v, isV := parseVault(blob); isV {
		dk := unwrapDK(v, "pass", deriveKey(name, pass))
		if dk == nil {
			dk = seDK(v, name, pass) // hardened (Endospore) identity: open via this device's secure-element slot
		}
		if dk == nil {
			return nil, nil, false // ref resolved but no slot opens -> treat as blank (defensive)
		}
		if v.Sig != "" && !verifyVault(v, dk) {
			fail("head signature invalid -- the store served a tampered head") // a signed head must verify
		}
		checkRollback(profileRefV2(name, pass), v.Version) // #80: anchor keyed by the stable v2 ref (matches bumpAnchor)
		return dk, getBlob(base, v.Manifest), true
	}
	return deriveKey(name, pass), blob, true // legacy: the head blob IS the sealed manifest; DK == old key
}

// ---- Vault operations (shared by the CLI verbs and the login daemon). Panic on store/crypto error. ----

// createVault enrolls a NEW vault profile (random DK, empty data) and returns the 12-word recovery code.
func createVault(base, name, pass string) string {
	if capMode() { // managed mode: buffer the initial vault writes, then one batch lease + cap-PUT (no creds)
		capBegin()
		defer capFlush()
	}
	dk := randBytes(32)
	if capMode() {
		capSetSession(name, pass, dk) // fold the (storefront-seeded) wallet into this create's flush
	}
	entropy, err := bip39.NewEntropy(128)
	check(err)
	mnemonic, err := bip39.NewMnemonic(entropy)
	check(err)
	var buf bytes.Buffer // empty profile == an empty tar sealed under DK (restore's legacy-tar branch -> 0 files)
	tw := tar.NewWriter(&buf)
	tw.Close()
	v := buildVault(dk, name, pass, entropy, postBlob(base, seal(dk, buf.Bytes())))
	v.Version = 1
	signVault(v, dk)
	vh := postBlob(base, serializeVault(v))
	putHead(base, name, pass, vh)
	putRef(base, v.RecoveryRef, vh)
	return mnemonic
}

// migrateVault upgrades a legacy (bare-manifest) profile to a vault IN PLACE -- DK = the old derived key, so
// NO data re-encryption -- and returns its new recovery code. ("", false) if it's already a vault / no ref.
func migrateVault(base, name, pass string) (string, bool) {
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return "", false
	}
	if _, isV := parseVault(getBlob(base, head)); isV {
		return "", false
	}
	entropy, err := bip39.NewEntropy(128)
	check(err)
	mnemonic, err := bip39.NewMnemonic(entropy)
	check(err)
	dk := deriveKey(name, pass) // legacy DK == the derived key
	v := buildVault(dk, name, pass, entropy, head) // Manifest = the existing sealed manifest
	v.Version = 1
	signVault(v, dk)
	vh := postBlob(base, serializeVault(v))
	putHead(base, name, pass, vh)
	putRef(base, v.RecoveryRef, vh)
	return mnemonic, true
}

// rotateVault re-wraps DK under a new passphrase (no data re-encryption) and invalidates the old ref.
func rotateVault(base, name, oldpass, newpass string) error {
	head := getRef(base, headKey(base, name, oldpass))
	if head == "" {
		return errors.New("wrong passphrase / no profile")
	}
	v, isV := parseVault(getBlob(base, head))
	if !isV {
		return errors.New("profile not migrated to a vault yet")
	}
	dk := unwrapDK(v, "pass", deriveKey(name, oldpass))
	if dk == nil {
		return errors.New("pass slot won't unwrap")
	}
	for i := range v.Slots {
		if v.Slots[i].Kind == "pass" {
			v.Slots[i].Wrapped = wrap(deriveKey(name, newpass), dk)
		}
	}
	v.Version++ // a rotation is a head change -> bump + re-sign so the new head is fresh + verifiable
	signVault(v, dk)
	vh := postBlob(base, serializeVault(v))
	putHead(base, name, newpass, vh) // writes the v2 head for the NEW pass + tombstones its legacy ref
	// invalidate the OLD passphrase at BOTH ref schemes -- neither resolves after a rotation
	newV2 := profileRefV2(name, newpass)
	for _, oldRef := range []string{profileRefV2(name, oldpass), profileRefLegacy(name, oldpass)} {
		if oldRef != newV2 {
			putRef(base, oldRef, "")
		}
	}
	if v.RecoveryRef != "" {
		putRef(base, v.RecoveryRef, vh)
	}
	return nil
}

// recoverVault resets the passphrase via the 12-word recovery code (the old pass ref is left as-is -- the
// recoverer doesn't know the old pass; fine for forgot-pass, see docs/recovery.md).
func recoverVault(base, name, mnemonic, newpass string) error {
	entropy, err := bip39.EntropyFromMnemonic(mnemonic)
	if err != nil {
		return errors.New("invalid recovery phrase")
	}
	rref := recoveryRef(name, entropy)
	head := getRef(base, rref)
	if head == "" {
		return errors.New("no profile for that name + phrase")
	}
	v, isV := parseVault(getBlob(base, head))
	if !isV {
		return errors.New("head is not a vault")
	}
	dk := unwrapDK(v, "recovery", kekRecovery(name, entropy))
	if dk == nil {
		return errors.New("recovery slot won't unwrap")
	}
	nv := buildVault(dk, name, newpass, entropy, v.Manifest) // same DK + entropy, new pass slot
	nv.Version = v.Version + 1 // carry the version forward (don't reset, or the anchor would flag a rollback)
	nv.History = v.History     // #58: a passphrase change repoints the head -> keep the rollback snapshots
	nv.HeadKind = v.HeadKind   // #58: carry the live head's kind forward
	nv.LastSeal = v.LastSeal
	signVault(nv, dk)
	vh := postBlob(base, serializeVault(nv))
	putHead(base, name, newpass, vh)
	putRef(base, rref, vh)
	return nil
}

// snapshotList returns a profile's retained rollback snapshots (#58), newest-first, or nil if the profile
// doesn't resolve / has no history. Read-only; used to populate the "Restore a snapshot" chooser.
func snapshotList(base, name, pass string) []headSnap {
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return nil
	}
	v, isV := parseVault(getBlob(base, head))
	if !isV {
		return nil
	}
	return v.History
}

// errNoSnapshot: the requested rollback version is no longer retained (it churned out of the last-K window
// before the user confirmed) -- a STALE-LIST case, distinct from a store/connection failure, so the UI can
// say "that version is no longer available, pick another" instead of blaming the connection (#58).
var errNoSnapshot = errors.New("no retained snapshot for that version")

// rollbackHead repoints a profile's live head to a RETAINED snapshot (#58 recovery). It is a FORWARD write
// (new version = current+1 carrying the snapshot's manifest), NOT a version regression, so the rollback
// anchor + #72 receipts still hold. targetVersion == 0 selects the NEWEST snapshot (the "last good head"
// used by the gate's auto-offer). Returns the new head version. Cap-mode writes ride a lease via capWrite.
func rollbackHead(base, name, pass string, targetVersion uint64) (uint64, error) {
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return 0, errors.New("no profile / wrong passphrase")
	}
	v, isV := parseVault(getBlob(base, head))
	if !isV {
		return 0, errors.New("legacy head has no snapshot history")
	}
	dk := unwrapDK(v, "pass", deriveKey(name, pass))
	if dk == nil {
		dk = seDK(v, name, pass) // hardened identity opens via the device's secure element
	}
	if dk == nil {
		return 0, errors.New("wrong passphrase")
	}
	if !verifyVault(v, dk) {
		return 0, errors.New("head signature invalid -- refusing to trust its snapshot list")
	}
	var target *headSnap
	for i := range v.History {
		if targetVersion == 0 || v.History[i].Version == targetVersion {
			target = &v.History[i]
			break
		}
	}
	if target == nil {
		return 0, errNoSnapshot // the version churned out of the window before the user confirmed (not a store error)
	}
	if target.Manifest == "" {
		return 0, errors.New("snapshot has no manifest")
	}
	newManifest, targetKind := target.Manifest, target.Kind
	capWrite(name, pass, dk, func() {
		pushSnapshot(v, newManifest, v.HeadKind) // keep the CURRENT (rolled-from) head as a rollback point too
		v.Manifest = newManifest       // repoint the data head to the chosen snapshot
		v.Version++                    // FORWARD write (not a regression) -> anchor/receipts stay happy
		v.HeadKind = targetKind        // the restored version keeps its original kind
		v.LastSeal = time.Now().Unix()
		signVault(v, dk)
		vh := postBlob(base, serializeVault(v))
		putHead(base, name, pass, vh)
		if v.RecoveryRef != "" {
			putRef(base, v.RecoveryRef, vh)
		}
		bumpAnchor(profileRefV2(name, pass), v.Version) // #80: the head just migrated to v2 -> anchor the v2 ref
	})
	return v.Version, nil
}

// enrollSE hardens an identity to this device's secure element (Endospore E.3b): add a device-specific `se`
// keyslot (DK wrapped under kekSE(name,pass,se_secret)) and DROP the pass-only slot, so the head is no longer
// openable by passphrase alone on any device -> a captured store ciphertext is no longer offline
// brute-forceable. The head stays at its usual ref (findable via headKey), but only the SE device (pass +
// se_secret) or the recovery mnemonic can open it. First enroll needs the pass slot to obtain DK; enrolling a
// SECOND device (after hardening) goes via recovery first (recover -> DK -> a fresh enroll adds another `se`
// slot, since unwrapDK iterates all `se` slots). The recovery slot is always preserved (roaming).
func enrollSE(base, name, pass string, seSecret []byte) error {
	if len(seSecret) == 0 {
		return errors.New("empty se_secret")
	}
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return errors.New("wrong passphrase / no profile")
	}
	v, isV := parseVault(getBlob(base, head))
	if !isV {
		return errors.New("profile is not a vault (migrate first)")
	}
	dk := unwrapDK(v, "pass", deriveKey(name, pass))
	if dk == nil {
		dk = unwrapDK(v, "se", kekSE(name, pass, seSecret)) // re-enroll the same device is idempotent-ish
	}
	if dk == nil {
		return errors.New("no pass/se slot opens (already hardened on another device? recover first)")
	}
	v.Slots = append(v.Slots, keyslot{Kind: "se", Wrapped: wrap(kekSE(name, pass, seSecret), dk)})
	kept := make([]keyslot, 0, len(v.Slots)) // harden: drop every pass-only slot
	for _, s := range v.Slots {
		if s.Kind != "pass" {
			kept = append(kept, s)
		}
	}
	v.Slots = kept
	v.Hardened = true
	v.Version++
	signVault(v, dk)
	vh := postBlob(base, serializeVault(v))
	putHead(base, name, pass, vh)
	putRef(base, v.RecoveryRef, vh)
	return nil
}

// resolveKeySE opens a hardened identity's head via its secure-element `se` slot (the explicit form of the
// NOWHERE_SE_SECRET path that resolveKey uses). (name,pass) locate the head; DK is unwrapped with
// kekSE(name,pass,se_secret). Mirrors resolveKey.
func resolveKeySE(base, name, pass string, seSecret []byte) (key, manifestBlob []byte, ok bool) {
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return nil, nil, false
	}
	v, isV := parseVault(getBlob(base, head))
	if !isV {
		return nil, nil, false
	}
	dk := unwrapDK(v, "se", kekSE(name, pass, seSecret))
	if dk == nil {
		return nil, nil, false
	}
	if v.Sig != "" && !verifyVault(v, dk) {
		fail("head signature invalid -- the store served a tampered head")
	}
	checkRollback(profileRefV2(name, pass), v.Version) // #80: anchor keyed by the stable v2 ref (matches bumpAnchor)
	return dk, getBlob(base, v.Manifest), true
}

// daemonVaultOp runs a vault op for the daemon, turning a panic (store error) into ok=false (reply BLANK).
func daemonVaultOp(fn func() error) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return fn() == nil
}

func itemPrio(name string) int {
	if p, err := strconv.Atoi(strings.SplitN(name, "-", 2)[0]); err == nil {
		return p
	}
	return 999
}

func seal(key, plaintext []byte) []byte {
	block, _ := aes.NewCipher(key)
	gcm, err := cipher.NewGCM(block)
	check(err)
	// Per-identity CONVERGENT nonce: derive it from the plaintext under the identity key (HMAC), so
	// unchanged data re-seals to byte-identical ciphertext -> same content hash -> the store dedupes it
	// (no re-upload). Keyed by `key` so the convergent hash is private to this identity (avoids the
	// cross-identity confirmation-of-file attack that unkeyed convergent encryption allows).
	mac := hmac.New(sha256.New, key)
	mac.Write(plaintext)
	nonce := mac.Sum(nil)[:gcm.NonceSize()]
	return gcm.Seal(nonce, nonce, plaintext, nil)
}

func unseal(key, blob []byte) []byte {
	block, _ := aes.NewCipher(key)
	gcm, err := cipher.NewGCM(block)
	check(err)
	ns := gcm.NonceSize()
	if len(blob) < ns {
		fail("blob too short")
	}
	pt, err := gcm.Open(nil, blob[:ns], blob[ns:], nil)
	check(err)
	return pt
}

// ---- CDC chunk compression (DIA-20260624-08) ----
// zstd shrinks compressible CDC chunks (app DBs, text) before seal -> less to transfer (already-compressed
// media barely moves). EncodeAll/DecodeAll are concurrent-safe, so the parallel seal/restore workers share
// one codec. A FIXED level keeps output DETERMINISTIC -> a chunk compresses to stable bytes -> the convergent
// seal still dedups it. Chunks are compressed-then-sealed; the chunkManifest Version (2 = zstd) tells restore
// to decompress. Off via NOWHERE_COMPRESS=0 -> manifests stay V1, fully backward-compatible (and a V1 manifest
// from before this change always restores raw). NOTE: a klauspost/zstd bump could change the bytes -> a
// one-time re-seal, like any format change.
var zstdEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
var zstdDec, _ = zstd.NewReader(nil)

func compressChunks() bool { return os.Getenv("NOWHERE_COMPRESS") != "0" } // default ON

// packChunk compresses c when `compress`; unpackChunk reverses it for a chunk from a V>=2 manifest.
func packChunk(c []byte, compress bool) []byte {
	if compress {
		return zstdEnc.EncodeAll(c, nil)
	}
	return c
}
func unpackChunk(b []byte, version int) ([]byte, error) {
	if version >= 2 {
		return zstdDec.DecodeAll(b, nil)
	}
	return b, nil
}

// ---- S3 backend (any S3-compatible store). Selected by base=="s3"; config from env. ----
var s3client *minio.Client
var s3bucket string

// normRegion defaults a *blank* S3 region to us-east-1 but passes any explicit value through unchanged.
// Cloudflare R2 requires region "auto" for valid signing, so it must NOT be remapped (B2 uses its zone,
// e.g. us-west-004; Filebase uses us-east-1). One helper for every store-client init so the rule can't
// drift between copies (it once did -- the cause of the R2 presign break).
func normRegion(r string) string {
	if r == "" {
		return "us-east-1"
	}
	return r
}

// workerMu serializes ALL nowhere_roamd interactions -- roam.req/roam.res are a single shared slot, so the
// periodic background seal (syncLoop) must never run concurrently with a socket-driven login/logout.
var workerMu sync.Mutex

// s3RetryBase is the exponential-backoff base for retryStore (tests shrink it to run fast).
var s3RetryBase = 200 * time.Millisecond

// retryStore runs a store op up to 5x with exponential backoff so a TRANSIENT failure -- a Wi-Fi blip, a
// Filebase 5xx/429, a connection reset mid-seal -- costs a retry instead of aborting the whole operation.
// This is the fix for the silent large-seal loss (DIA-20260625-01): pushProfile uploads every chunk BEFORE
// it writes the manifest+ref, so a SINGLE un-retried PUT error panicked out and discarded the entire seal --
// the chunks already uploaded were orphaned and the ref was left at the prior (pre-change) snapshot, so the
// next login restored stale data ("300 MB roam silently lost"). A 9 MB seal (a few PUTs) finished before any
// blip; a 300 MB seal (hundreds of concurrent PUTs over Wi-Fi) reliably caught one. Store ops are
// content-addressed + idempotent (PUT) or read-only (GET/STAT), so retrying is always safe. `op` returns nil
// on success OR a definitive non-retryable outcome (e.g. a real 404, captured by the caller), or a non-nil
// error to retry; the final error is returned so a genuinely-down store still fails LOUDLY rather than
// silently dropping the seal.
func retryStore(what string, op func() error) error {
	const attempts = 5
	var err error
	for i := 0; ; i++ {
		if err = op(); err == nil || i == attempts-1 {
			return err
		}
		fmt.Fprintf(os.Stderr, "  retry %s (%d/%d): %v\n", what, i+2, attempts, err)
		time.Sleep(s3RetryBase << i)
	}
}

func s3Init() {
	if s3client != nil {
		return
	}
	ep := os.Getenv("S3_ENDPOINT")
	secure := !strings.HasPrefix(ep, "http://")
	host := strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	region := normRegion(os.Getenv("S3_REGION"))
	s3bucket = os.Getenv("S3_BUCKET")
	c, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), ""),
		Secure: secure,
		Region: region,
	})
	check(err)
	s3client = c
}

func s3Get(key string) ([]byte, bool) {
	s3Init()
	var b []byte
	miss := false
	check(retryStore("GET "+key, func() error {
		obj, err := s3client.GetObject(context.Background(), s3bucket, key, minio.GetObjectOptions{})
		if err != nil {
			return err
		}
		defer obj.Close()
		data, err := io.ReadAll(obj)
		if err != nil {
			er := minio.ToErrorResponse(err)
			if er.Code == "NoSuchKey" || er.StatusCode == 404 {
				miss = true // definitively absent -> not an error, do not retry
				return nil
			}
			return err
		}
		b = data
		return nil
	}))
	if miss {
		return nil, false
	}
	return b, true
}

func s3Put(key string, data []byte) {
	s3Init()
	check(retryStore("PUT "+key, func() error {
		_, err := s3client.PutObject(context.Background(), s3bucket, key, bytes.NewReader(data),
			int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
		return err
	}))
}

// s3Size returns a blob's stored byte size via a STAT (no download), or (0,false) if absent/error. Used by the
// billing footprint: the gateway leases per-ref by size, so we need each chunk's size without fetching it.
func s3Size(key string) (int64, bool) {
	s3Init()
	var size int64
	ok := false
	retryStore("STAT "+key, func() error {
		oi, err := s3client.StatObject(context.Background(), s3bucket, key, minio.StatObjectOptions{})
		if err != nil {
			er := minio.ToErrorResponse(err)
			if er.Code == "NoSuchKey" || er.StatusCode == 404 {
				return nil
			}
			return err
		}
		size = oi.Size
		ok = true
		return nil
	})
	return size, ok
}

// blobSize returns a blob's size for ANY store: a cheap STAT on S3, or len(getBlob) elsewhere (the HTTP test
// store has no stat). Returns 0 when absent. (DIA-20260626-B)
// capSize returns a blob's stored size (bytes) via a SIZE-ONLY ranged GET on its presigned cap URL -- no full
// download. A cap URL is presigned for GET, and SigV4 binds the method, so a HEAD would fail the signature;
// instead we GET with `Range: bytes=0-0` and read the total from Content-Range ("bytes 0-0/<total>"), pulling
// ≤1 byte of body. Returns -1 on any error / non-range store, so blobSize falls back to a full read. (#87 --
// without this, sizing a cap-mode blob DOWNLOADED the whole chunk, so GET-USAGE/footprint re-fetched the whole
// profile.) key is the full store key, e.g. "blob/<hash>".
func capSize(key string) int64 {
	u, err := capReadURL(key)
	if err != nil {
		return -1
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return -1
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := storeHTTP.Do(req)
	if err != nil {
		capInvalidate(key) // a stale cached presign -> force a fresh one next time
		return -1
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusPartialContent { // 206: "Content-Range: bytes 0-0/<total>"
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, perr := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64); perr == nil && n >= 0 {
					return n
				}
			}
		}
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		return resp.ContentLength // store ignored Range (rare) -> trust Content-Length
	}
	return -1
}

func blobSize(base, ref string) int64 {
	if isS3(base) && !capMode() {
		sz, _ := s3Size("blob/" + ref) // blobs live at blob/<hash>, NOT the bare hash (the old bug: 0 size)
		return sz
	}
	if capMode() {
		if sz := capSize("blob/" + ref); sz >= 0 { // #87: size via a 1-byte ranged GET, not a full chunk download
			return sz
		}
	}
	return int64(len(getBlob(base, ref))) // plain HTTP store, or a cap size-probe that failed -> full read
}

func s3Exists(key string) bool {
	s3Init()
	exists := false
	// Best-effort (the result only gates a skip-if-exists dedup): a real 404 -> absent; a transient error ->
	// retry; on a persistent transient error fall through to exists=false so the caller re-PUTs (safe -- a
	// re-PUT of identical content-addressed data is a harmless overwrite), never panicking the seal over a stat.
	retryStore("STAT "+key, func() error {
		_, err := s3client.StatObject(context.Background(), s3bucket, key, minio.StatObjectOptions{})
		if err != nil {
			er := minio.ToErrorResponse(err)
			if er.Code == "NoSuchKey" || er.StatusCode == 404 {
				return nil
			}
			return err
		}
		exists = true
		return nil
	})
	return exists
}

func isS3(base string) bool { return base == "s3" }

// ---- cap-gated store I/O (managed / least-knowledge mode, DIA-20260627). When GATEWAY_URL is set and NO
// S3 credentials are present, the device holds NO store creds: it routes blob/ref reads + writes through the
// gateway's presigned caps (billing.go readCap/writeCap) against the gateway's managed store. Reads are free;
// writes require a live lease. Self-hosted devices keep S3_* creds and use the direct s3Get/s3Put path. ----

func capMode() bool {
	return os.Getenv("GATEWAY_URL") != "" && os.Getenv("S3_ACCESS_KEY") == ""
}

type capEntry struct {
	url string
	exp time.Time
}

var capReadMu sync.Mutex
var capReadCache = map[string]capEntry{}

// capReadURL returns a presigned GET URL for key, cached briefly (well under the gateway's cap TTL) so a
// restore that re-reads the same ref doesn't re-fetch a cap each time. A presigned URL is key-scoped (not a
// content snapshot), so a cached URL still reads the CURRENT object -- safe for mutable head refs too.
// Returns the gateway error (a transient /cap blip) instead of check-failing so capGet can RETRY it (#70).
func capReadURL(key string) (string, error) {
	capReadMu.Lock()
	if e, ok := capReadCache[key]; ok && time.Now().Before(e.exp) {
		u := e.url
		capReadMu.Unlock()
		return u, nil
	}
	capReadMu.Unlock()
	u, err := newGatewayClient(os.Getenv("GATEWAY_URL")).readCap(key)
	if err != nil {
		return "", err
	}
	capReadMu.Lock()
	capReadCache[key] = capEntry{url: u, exp: time.Now().Add(30 * time.Second)}
	capReadMu.Unlock()
	return u, nil
}

// storeHTTP is the client for STORE data I/O (the presigned chunk GET/PUT). Unlike the stdlib default client it
// has a per-request TIMEOUT, so a hung or stale pooled connection can't wedge FOREVER -- the request times out
// and the caller's retry (retryStore) re-tries, instead of blocking the seal/restore indefinitely. That block
// was the root of two on-device freezes: the daemon stuck on a dead store connection stopped answering its
// socket clients, which stranded the logoff reap AND ANR'd the gate's boot-wipe (#47). 120s is generous --
// only a genuine hang trips it, not a slow multi-MB chunk transfer. Rides the pooled DefaultTransport (nil
// Transport), so #71's keep-alive win is kept. 60s: a single chunk is <= cdcMax (4 MiB), so even a very slow
// link finishes in seconds -- only a genuine hang reaches the timeout.
var storeHTTP = &http.Client{Timeout: 60 * time.Second}

// capInvalidate drops any cached cap for key so the next capReadURL re-fetches a fresh one -- called on a GET
// failure in case the cached presign was the problem (edge expiry / revocation), not just a network blip (#70).
func capInvalidate(key string) {
	capReadMu.Lock()
	delete(capReadCache, key)
	capReadMu.Unlock()
}

// prefetchReadCaps warms capReadCache with read caps for keys IN BULK (one /caps POST per batch of 512), so a
// restore's per-chunk capReadURL hits the cache instead of paying a per-chunk gateway round-trip -- the round
// trip is ~half of each chunk's fetch time, so this is the big remaining login-speed win (#71 step 2). The
// gateway's cap TTL (15m) far outlasts any restore, so caching the whole set upfront is safe. Best-effort: any
// error (a store hiccup, or an OLD gateway with no /caps -> non-200) just STOPS prefetching, and the download
// falls back to per-chunk readCap -- correctness never depends on the batch endpoint.
func prefetchReadCaps(keys []string) {
	if !capMode() || len(keys) == 0 {
		return
	}
	gw := newGatewayClient(os.Getenv("GATEWAY_URL"))
	const batch = 512
	for i := 0; i < len(keys); i += batch {
		end := i + batch
		if end > len(keys) {
			end = len(keys)
		}
		caps, ttl, err := gw.readCaps(keys[i:end])
		if err != nil || len(caps) == 0 {
			fmt.Fprintf(os.Stderr, "  cap prefetch stopped at %d/%d (%v) -> per-chunk fallback\n", i, len(keys), err)
			return
		}
		dur := time.Duration(ttl) * time.Second
		if dur <= 0 {
			dur = 30 * time.Second
		}
		exp := time.Now().Add(dur * 3 / 4) // cache for 3/4 of the presign lifetime -> a margin before it expires
		capReadMu.Lock()
		for ref, u := range caps {
			capReadCache[ref] = capEntry{url: u, exp: exp}
		}
		capReadMu.Unlock()
	}
}

// capGet GETs the object at key via a free read cap. Returns (data, true) on 200; (nil, false) on 404/403
// so callers distinguish a missing ref/blob (a wrong-passphrase ref MUST read as absent -> blind login)
// from a transport/5xx error, which fails loudly.
func capGet(key string) ([]byte, bool) {
	// RETRY the whole read (gateway /cap fetch + presigned GET + body read) with backoff so a transient blip --
	// the "software caused connection abort" that used to dump a mid-restore login back to the gate -- costs a
	// retry, not the whole restore (#70). Mirrors s3Get: a 404/403 is a DEFINITIVE absence (miss), never retried,
	// so a wrong-passphrase ref still reads as absent for blind login; only transport/5xx errors retry.
	var out []byte
	var miss bool
	err := retryStore("cap GET "+key, func() error {
		miss, out = false, nil
		u, uerr := capReadURL(key)
		if uerr != nil {
			return uerr // transient gateway /cap error -> retry
		}
		resp, gerr := storeHTTP.Get(u)
		if gerr != nil {
			capInvalidate(key) // the cached cap may be stale -> force a fresh one on the next attempt
			return gerr
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
			miss = true // missing key (some S3-compatible stores 403 rather than 404 to hide existence)
			return nil  // definitive absence -> not an error, do not retry
		}
		if resp.StatusCode != http.StatusOK {
			capInvalidate(key)
			return fmt.Errorf("cap GET %s -> status %d", key, resp.StatusCode)
		}
		b, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return rerr // a read that fails mid-body (connection reset) -> retry the whole GET
		}
		out = b
		return nil
	})
	check(err) // still-failing after all attempts -> a genuinely-down store, fail LOUDLY (not a silent miss)
	return out, !miss
}

// ---- cap-gated WRITES (P3, Option A): seal + lease are COUPLED. In cap mode postBlob/putRef BUFFER the
// object (objectKey -> bytes) within a capBegin/capFlush bracket; capFlush BATCH-leases all buffered keys
// (one payment, sized by their bytes) and PUTs each via its returned write cap. So new data is paid-for
// exactly when written, and the device never holds store creds. A cap-mode write OUTSIDE a bracket fails
// loudly -- we never silently fall back (there are no creds) nor leave a write untracked/unpaid. ----

var capWriteMu sync.Mutex
var capBuffering bool

// cap-mode streaming write state (DIA-20260630-19, #45): replaces the old in-RAM capWriteBuf map that held
// the ENTIRE seal in memory (OOM'd the device on a multi-GB session). Sealed BLOB bytes are spilled to a disk
// file (NOT the tmpfs state dir) and indexed by {key, off, size}; tiny ref POINTERS are held in RAM and PUT
// LAST so the profile head appears only after every blob is confirmed -- a partial/failed flush can never
// clobber a good head (the data-loss bug). RAM stays bounded to one chunk regardless of seal size.
type capBlobRef struct {
	key       string
	off, size int64
}
type capState struct {
	spillPath string
	spill     *os.File
	off       int64
	blobs     []capBlobRef
	seen      map[string]bool   // content-addressed blob-key dedup within the bracket (a repeated chunk spills once)
	refs      map[string][]byte // ref pointers, PUT last (atomic head)
}

var capCur *capState

// capSession carries the live identity's key material so capFlush can FOLD the wallet: after taking the data
// tokens it re-seals the POST-SPEND wallet and writes it in the SAME lease, so the tiny wallet ref rounds in
// for free (no per-write charge, no self-reference). Set by the seal ops (pushProfile/createVault).
type capSessionT struct {
	active     bool
	name, pass string
	dk         []byte
}

var capSession capSessionT

func capSetSession(name, pass string, dk []byte) {
	capWriteMu.Lock()
	capSession = capSessionT{active: true, name: name, pass: pass, dk: append([]byte(nil), dk...)}
	capWriteMu.Unlock()
}

func capBegin() {
	capWriteMu.Lock()
	defer capWriteMu.Unlock()
	f, err := os.CreateTemp(spillDir(), "capspill-")
	check(err)
	capBuffering = true
	capCur = &capState{spillPath: f.Name(), spill: f, seen: map[string]bool{}, refs: map[string][]byte{}}
	capSession = capSessionT{}
}

// capBufferPut records one object write; false if not currently buffering (the caller skipped capBegin --
// a cap-mode write outside a bracket, which is a bug the primitives turn into a loud failure). A ref pointer
// (tiny) is held in RAM and PUT last; a blob is SPILLED to disk and indexed, so RAM never holds the seal.
func capBufferPut(key string, data []byte) bool {
	capWriteMu.Lock()
	defer capWriteMu.Unlock()
	if !capBuffering || capCur == nil {
		return false
	}
	if strings.HasPrefix(key, "ref/") {
		capCur.refs[key] = append([]byte(nil), data...) // pointer -> RAM, PUT last (atomic head)
		return true
	}
	if capCur.seen[key] {
		return true // already spilled this content-addressed blob in this bracket
	}
	n, err := capCur.spill.Write(data) // spill to disk; do NOT retain in RAM (the OOM fix)
	check(err)
	capCur.seen[key] = true
	capCur.blobs = append(capCur.blobs, capBlobRef{key: key, off: capCur.off, size: int64(n)})
	capCur.off += int64(n)
	return true
}

// capWalletPath locates the session wallet that pays for a flush -- the tmpfs state dir on-device, or an
// explicit override for the CLI/tests.
func capWalletPath() string {
	if w := os.Getenv("NOWHERE_WALLET"); w != "" {
		return w
	}
	if s := os.Getenv("NOWHERE_STATE"); s != "" {
		return filepath.Join(s, "wallet.json")
	}
	return "wallet.json"
}

// spillDir is where capFlush spills sealed blob bytes -- MUST be on DISK, not the tmpfs state dir, so a
// multi-GB seal can't blow up RAM. The roam worker sets NOWHERE_SPILL to a /data path with space; the
// CLI/tests fall back to the OS temp dir.
func spillDir() string {
	if d := os.Getenv("NOWHERE_SPILL"); d != "" {
		return d
	}
	return os.TempDir()
}

// capFlush ends the bracket: batch-lease all buffered object keys (one lease = one payment, owed by their
// total bytes) from the session wallet, then PUT each to its returned write cap. A missing cap or failed PUT
// is fatal -- in cap mode there are no creds to fall back to, so a lost write must never pass silently.
func capFlush() {
	capWriteMu.Lock()
	st := capCur
	capCur = nil
	capBuffering = false
	sess := capSession
	capSession = capSessionT{}
	capWriteMu.Unlock()
	if st == nil {
		return
	}
	defer os.Remove(st.spillPath)
	defer st.spill.Close()
	if len(st.blobs) == 0 && len(st.refs) == 0 {
		return
	}
	// The scan/buffer pass is done; everything below is the real upload to the store. Flip the gate's logoff
	// label to "Saving your data… X%" NOW -- before the lease round-trip -- so it doesn't sit at "Preparing…
	// 100%" during the lease, and so a big buffered upload advances a bar the whole way. total = the buffered
	// blob bytes we're about to PUT (DIA-20260701-01).
	setProgPhase("save")
	var total, uploaded int64
	for _, b := range st.blobs {
		total += b.size
	}
	writeProg(0, int(total))
	gw := newGatewayClient(os.Getenv("GATEWAY_URL"))
	q, err := gw.quote()
	check(err)
	wpath := capWalletPath()
	w, err := loadWallet(wpath)
	check(err)
	// Lease ref-set: every spilled blob (by size) + every pointer. One lease, sized by total bytes -- same
	// token math as before; spilling changed only WHERE the bytes live, not how many tokens they cost.
	refs := make(map[string]int64, len(st.blobs)+len(st.refs)+2)
	for _, b := range st.blobs {
		refs[b.key] = b.size
	}
	for k, v := range st.refs {
		refs[k] = int64(len(v))
	}
	owed := owedTokens(q, refs)
	// FOLD (DIA-20260627): re-seal the wallet into THIS batch so it rides the data lease for free (its tiny
	// ref rounds into the per-GiB token math). The wallet BLOB is small -> keep it in RAM; its pointer joins
	// the refs and is PUT last like any head. Factored so the paid and free paths each fold their own wallet.
	var foldKey string
	var foldBlob []byte
	doFold := func(wal *Wallet) {
		if !sess.active || sess.dk == nil {
			return
		}
		wj, _ := json.Marshal(wal)
		foldBlob = seal(sess.dk, wj)
		sum := sha256.Sum256(foldBlob)
		h := hex.EncodeToString(sum[:])
		foldKey = "blob/" + h
		refs[foldKey] = int64(len(foldBlob))
		// #80: write the wallet head at the v2 ref (its migration target -- headKey prefers v2). A pre-#80 legacy
		// wallet ref is left unleased here and reaped by store GC; reads never consult it once v2 exists.
		wref := profileRefV2(sess.name+"#wallet", sess.pass)
		st.refs["ref/"+wref] = []byte(h)
		refs["ref/"+wref] = int64(len(h))
	}
	var info leaseInfo
	if plain, blind, ok := w.choosePayment(owed); ok { // PAID: spend tokens, fold the post-spend wallet
		doFold(w)
		var code int
		info, code, err = gw.lease(plain, blind, refs)
		check(err)
		if code != http.StatusOK {
			fail(fmt.Sprintf("cap-flush lease: HTTP %d", code))
		}
		w.settlePayment(plain, blind, info.Spent) // delta lease: keep the tokens the gateway didn't spend
		check(saveWallet(wpath, w))
	} else { // no/insufficient credit -> FREE tier: token-less lease metered per profile (#54), up to quota.
		if !sess.active { // a credit-less throwaway has no profileRef to meter a free lease under
			panic(errInsufficientCredit)
		}
		w, err = loadWallet(wpath) // discard choosePayment's partial take; the free path spends nothing
		check(err)
		doFold(w)
		var code int
		info, code, err = gw.leaseFree(profileRefV2(sess.name, sess.pass), refs) // #80: meter the free tier under the v2 ref (the head the seal writes)
		check(err)
		if code == http.StatusPaymentRequired {
			panic(errInsufficientCredit) // over the free quota -> the user genuinely needs paid credit
		}
		if code != http.StatusOK {
			fail(fmt.Sprintf("cap-flush free lease: HTTP %d", code))
		}
		check(saveWallet(wpath, w)) // wallet unchanged by a free lease; keep the on-disk copy authoritative
	}
	put := func(key string, data []byte) {
		u := info.WriteCaps[key]
		if u == "" {
			fail("cap-flush: gateway returned no write cap for " + key)
		}
		capPut(u, data)
		if strings.HasPrefix(key, "blob/") {
			blobCacheAdd(key[len("blob/"):]) // confirmed in the store -> a later unchanged seal skips it
		}
	}
	// 1) PUT every data blob, STREAMED from the spill file -- RAM bounded to one chunk (the OOM fix). capPut
	//    is synchronous, so the read buffer is safe to reuse across blobs. Emit upload progress as we go: the
	//    seal's own ticker has already stopped by the time this DEFERRED flush runs, so without this a large
	//    streamed upload reads as "no progress" and the worker's 150s stall watchdog trips a FALSE ERR-TIMEOUT
	//    -> the misleading "Not backed up" even though the upload is fine (#45). total/uploaded were computed
	//    above so we could flip the label to "save" before the lease (DIA-20260701-01).
	var rbuf []byte
	for _, b := range st.blobs {
		if int64(cap(rbuf)) < b.size {
			rbuf = make([]byte, b.size)
		}
		data := rbuf[:b.size]
		if _, err := st.spill.ReadAt(data, b.off); err != nil {
			fail("cap-flush: spill read: " + err.Error())
		}
		put(b.key, data)
		uploaded += b.size
		writeProg(int(uploaded), int(total)) // keep the stall watchdog fed during the upload (no-op off-device)
	}
	// 2) PUT the folded wallet blob (small, in RAM).
	if foldKey != "" {
		put(foldKey, foldBlob)
	}
	// 3) PUT the pointers LAST -- the profile head appears only AFTER every blob above is confirmed. So a
	//    partial / failed flush leaves the previous head intact: a crash can never clobber a good head with
	//    a half-written one (the #45 data-loss fix; cf. a/a1234 going BLANK after an OOM'd seal).
	for k, v := range st.refs {
		put(k, v)
	}
}

// capAbort tears down a cap bracket that did NOT reach capFlush -- e.g. a seal write panicked mid-bracket.
// It drops the spill file and clears the buffering flag so a LONG-LIVED process (the login daemon, which
// runs handleExport inline) can't leak a spill or leave capBuffering stuck true for the next write. It is a
// no-op after a successful capFlush (which already niled capCur), so callers can `defer capAbort()` safely.
func capAbort() {
	capWriteMu.Lock()
	st := capCur
	capCur = nil
	capBuffering = false
	capSession = capSessionT{}
	capWriteMu.Unlock()
	if st != nil {
		st.spill.Close()
		os.Remove(st.spillPath)
	}
}

// capWrite runs a small daemon-inline head write (export bundle, snapshot rollback) through a cap lease when
// in managed mode (the device holds no store creds), else directly (self-hosted S3 creds). It brackets fn's
// postBlob/putRef with capBegin/capFlush and hands capFlush the session key material so it pays from the
// roaming wallet; capAbort (deferred) drops the spill if fn panics mid-bracket. For SMALL writes only (a head
// vault + its ref) -- the big seal path uses pushProfile's own bracket. Panics from fn/capFlush propagate.
func capWrite(name, pass string, dk []byte, fn func()) {
	if !capMode() {
		fn()
		return
	}
	capBegin()
	defer capAbort()
	capSetSession(name, pass, dk)
	fn()
	capFlush()
}

func capPut(capURL string, data []byte) {
	// storeHTTP has a timeout, so a hung/stale connection returns an error instead of wedging; retryStore then
	// re-tries a transient failure (like capGet) so one blip doesn't fail the whole seal. A content-addressed
	// PUT is idempotent, so retrying is always safe.
	check(retryStore("cap PUT", func() error {
		req, _ := http.NewRequest(http.MethodPut, capURL, bytes.NewReader(data))
		resp, err := storeHTTP.Do(req)
		if err != nil {
			return err // timeout / transport -> retry
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			b, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("cap PUT -> %d: %s", resp.StatusCode, short(string(b)))
		}
		return nil
	}))
}

// --- Known-present blob cache (DIA-20260625-08). A confirmed seal re-checks EVERY chunk against the store
// (postBlob -> s3Exists), so an UNCHANGED ~300-chunk profile does ~300 network round-trips even though it
// uploads nothing -- a 30-60s "why is logoff slow when I changed nothing". This remembers the hashes we've
// CONFIRMED are in the store (a stat hit or a successful PUT) so a later seal skips the network stat for them.
// SAFETY: it only ever skips the existence CHECK, never the upload of an unconfirmed chunk; and it's keyed by
// store (a different store uses a different file) so it can never make us skip uploading a chunk the live store
// lacks. Lives in the RAM state dir, so it's re-populated by the first periodic seal after each boot (a
// background sync -- the user-facing logoff is already warm). Opt-in via NOWHERE_BLOBCACHE: unset (the daemon
// process, VM tests) -> no file caching, i.e. the original always-stat behaviour.
var blobCache map[string]bool
var blobCacheMu sync.Mutex // postBlob runs in concurrent upload goroutines -> guard the map (a cold seal writes it)
var blobCacheHits int64    // chunks skipped via the cache this process (diagnostic, logged by pushProfile)

func blobCachePath() string {
	dir := os.Getenv("NOWHERE_BLOBCACHE")
	if dir == "" {
		return ""
	}
	tag := sha256.Sum256([]byte(os.Getenv("S3_ENDPOINT") + "\x00" + os.Getenv("S3_BUCKET")))
	return filepath.Join(dir, "blobcache."+hex.EncodeToString(tag[:])[:12])
}

// blobCacheInit loads the on-disk set once. Caller holds blobCacheMu.
func blobCacheInit() {
	if blobCache != nil {
		return
	}
	blobCache = map[string]bool{}
	if p := blobCachePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					blobCache[line] = true
				}
			}
		}
	}
}

// blobCacheKnown reports whether key is already confirmed present in this store.
func blobCacheKnown(key string) bool {
	blobCacheMu.Lock()
	defer blobCacheMu.Unlock()
	blobCacheInit()
	if blobCache[key] {
		atomic.AddInt64(&blobCacheHits, 1)
		return true
	}
	return false
}

// blobCacheAdd records a chunk hash as confirmed present (append to the per-store file + the in-memory set).
// Also called at RESTORE to SEED the cache from the manifest -- those chunks are referenced by the live ref, so
// they're present -- which warms it from login, so the first seal (and a quick logoff) skip the network too.
func blobCacheAdd(key string) {
	blobCacheMu.Lock()
	defer blobCacheMu.Unlock()
	blobCacheInit()
	if blobCache[key] {
		return // already known -> nothing to append
	}
	blobCache[key] = true
	if p := blobCachePath(); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.WriteString(key + "\n")
			f.Close()
		}
	}
}

// Content cache (DIA-20260626-01): the blob cache above skips the per-chunk network STAT for known chunks, but a
// no-change seal still re-runs the PURE LOCAL CRYPTO (zstd-compress + encrypt + sha256) over the whole tree (~27s
// on a 2.6GB profile). This maps the CHEAP plaintext-chunk-hash -> the sealed-blob-hash, so an UNCHANGED tar-stream
// chunk reuses its sealed form and skips packChunk+seal+postBlob (only the read+CDC-chunk floor remains). The sealed
// hash = f(DK, plaintext), so it's keyed by store+DK; RAM (state dir, NOWHERE_BLOBCACHE), seeded at restore from the
// decrypted chunks (warm from login). SAFETY: a hit only skips when blobCacheKnown(sealed) ALSO confirms the blob is
// in the store -- so we can never repoint a head at a GC'd/missing blob (a wrong skip = data loss). Opt-in (no dir
// set -> always full crypto, e.g. the daemon / VM tests).
// PRIVACY: the entries are plaintext-chunk-HASHES (a content fingerprint, more sensitive than the blob cache's
// sealed hash since it needs no DK), but the dir is the RAM tmpfs ($STATE) -- wiped on power-off, never roamed --
// and while powered on the plaintext itself is already live in /data/user/N, so the marginal exposure is nil.
var contentCache map[string]string // plaintext-chunk-hash -> sealed-blob-hash
var contentCacheMu sync.Mutex
var contentCacheHits int64
var contentCacheLoadedTag string // the store+DK tag the in-memory map currently holds (reloaded when the DK changes)

func contentCacheTagFor(dk []byte) string {
	t := sha256.Sum256(append([]byte(os.Getenv("S3_ENDPOINT")+"\x00"+os.Getenv("S3_BUCKET")+"\x00"), dk...))
	return hex.EncodeToString(t[:])[:12]
}
func contentCachePath(tag string) string {
	dir := os.Getenv("NOWHERE_BLOBCACHE")
	if dir == "" || tag == "" {
		return ""
	}
	return filepath.Join(dir, "contentcache."+tag)
}

// contentCacheInit (re)loads the on-disk map when the DK tag changes. Caller holds contentCacheMu.
func contentCacheInit(tag string) {
	if contentCache != nil && contentCacheLoadedTag == tag {
		return
	}
	contentCache = map[string]string{}
	contentCacheLoadedTag = tag
	if p := contentCachePath(tag); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if f := strings.Fields(line); len(f) == 2 {
					contentCache[f[0]] = f[1]
				}
			}
		}
	}
}

// contentCacheGet returns the sealed-blob-hash for a plaintext-chunk-hash under this DK tag, or "" on a miss.
// (contentCacheHits counts ACTUAL crypto-skips -- a map hit that the blobCacheKnown gate then rejects is NOT a
// skip -- so the caller increments it only when it really reuses the sealed form, not here.)
func contentCacheGet(tag, plain string) string {
	contentCacheMu.Lock()
	defer contentCacheMu.Unlock()
	contentCacheInit(tag)
	return contentCache[plain]
}

// contentCachePut records plain -> sealed (append to the per-DK file + the in-memory map). Called at restore (seed)
// and after a real seal.
func contentCachePut(tag, plain, sealed string) {
	contentCacheMu.Lock()
	defer contentCacheMu.Unlock()
	contentCacheInit(tag)
	if contentCache[plain] == sealed {
		return
	}
	contentCache[plain] = sealed
	if p := contentCachePath(tag); p != "" {
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.WriteString(plain + " " + sealed + "\n")
			f.Close()
		}
	}
}

// contentKey binds a plaintext-chunk-hash to the manifest VERSION, because the sealed encoding depends on it
// (V2 = zstd-then-seal, V1 = seal only): the same plaintext yields a DIFFERENT sealed blob per version, so they
// must never collide in the cache -- a cross-version hit would repoint a V1 manifest at a V2 blob (or vice versa)
// and restore would mis-decode it (corrupt tar). (DIA-20260626-01)
func contentKey(version int, plainHash string) string { return strconv.Itoa(version) + "." + plainHash }

// --- Discovery store: a SEPARATE S3 endpoint (DISCO_* env, baked) the device reaches to LOOK UP its data
// store config -- so a fresh device with no S3_* can still bootstrap. Its own client/bucket, not the data
// s3client. Reached via the "disco" base in getRef/putRef. ---
var discoClient *minio.Client
var discoBucket string

// discoCanLookup: enough config to LOOK UP a sealed store-config -- just the endpoint + bucket. Discovery
// only ever holds sealed (zero-knowledge) blobs at unguessable bootstrapRefs, so the GET can be ANONYMOUS
// against a public-read bucket -> a fresh device bootstraps with NO baked creds at all (the leaked-image
// risk of a baked account-level key is gone from the bootstrap path).
func discoCanLookup() bool {
	return os.Getenv("DISCO_ENDPOINT") != "" && os.Getenv("DISCO_BUCKET") != ""
}

// discoConfigured: full creds -> can also PUBLISH (PUT) to discovery. Publishing happens on a device that
// already has the data store configured (so it holds write creds anyway); the keyless lookup above is the
// fresh-device path. (Scoped, least-privilege WRITE tokens are the next step -- the Phase-2 broker.)
func discoConfigured() bool {
	return discoCanLookup() &&
		os.Getenv("DISCO_ACCESS_KEY") != "" && os.Getenv("DISCO_SECRET_KEY") != ""
}

// discoURL builds the path-style public object URL for an anonymous GET (e.g. https://s3.filebase.io/<bucket>/<key>).
func discoURL(key string) string {
	return strings.TrimRight(os.Getenv("DISCO_ENDPOINT"), "/") + "/" + os.Getenv("DISCO_BUCKET") + "/" + key
}

func discoInit() {
	if discoClient != nil {
		return
	}
	ep := os.Getenv("DISCO_ENDPOINT")
	secure := !strings.HasPrefix(ep, "http://")
	host := strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	region := normRegion(os.Getenv("DISCO_REGION"))
	discoBucket = os.Getenv("DISCO_BUCKET")
	c, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("DISCO_ACCESS_KEY"), os.Getenv("DISCO_SECRET_KEY"), ""),
		Secure: secure,
		Region: region,
	})
	check(err)
	discoClient = c
}

func discoGet(key string) ([]byte, bool) {
	if os.Getenv("DISCO_ACCESS_KEY") == "" {
		return discoGetAnon(key) // no creds baked -> anonymous public-read lookup
	}
	discoInit()
	obj, err := discoClient.GetObject(context.Background(), discoBucket, key, minio.GetObjectOptions{})
	check(err)
	defer obj.Close()
	b, err := io.ReadAll(obj)
	if err != nil {
		er := minio.ToErrorResponse(err)
		if er.Code == "NoSuchKey" || er.StatusCode == 404 {
			return nil, false
		}
		check(err)
	}
	return b, true
}

// discoGetAnon fetches a discovery blob over a plain (credential-free) HTTP GET against a public-read
// bucket. Any non-200 is a miss -> false (blind: a wrong name/pass, an absent ref, or a non-public bucket
// are all indistinguishable to the caller; the device just falls back to NOSTORE). A 403 is logged as a
// hint that the discovery bucket may not be public-read.
func discoGetAnon(key string) ([]byte, bool) {
	resp, err := http.Get(discoURL(key))
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 403 {
			fmt.Fprintf(os.Stderr, "[disco] anon GET %s -> 403 (is the discovery bucket public-read?)\n", key)
		}
		return nil, false
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return b, true
}

func discoPut(key string, data []byte) {
	discoInit()
	_, err := discoClient.PutObject(context.Background(), discoBucket, key, bytes.NewReader(data),
		int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	check(err)
}

func getRef(base, ref string) string {
	if base == "disco" {
		b, ok := discoGet("ref/" + ref)
		if !ok {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	if isS3(base) {
		key := "ref/" + ref
		var b []byte
		var ok bool
		if capMode() { // managed mode: read the pointer via a free cap (no store creds)
			b, ok = capGet(key)
		} else {
			b, ok = s3Get(key)
		}
		if !ok {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	resp, err := http.Get(base + "/ref/" + ref)
	check(err)
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return ""
	}
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

func getBlob(base, key string) []byte {
	if isS3(base) {
		blobKey := "blob/" + key
		var b []byte
		var ok bool
		if capMode() { // managed mode: read the blob via a free cap (no store creds)
			b, ok = capGet(blobKey)
		} else {
			b, ok = s3Get(blobKey)
		}
		if !ok {
			fail("blob not found: " + key)
		}
		return b
	}
	resp, err := http.Get(base + "/blob/" + key)
	check(err)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail(fmt.Sprintf("GET blob %s -> status %d", key, resp.StatusCode))
	}
	b, _ := io.ReadAll(resp.Body)
	return b
}

func postBlob(base string, data []byte) string {
	if isS3(base) {
		h := sha256.Sum256(data)
		key := hex.EncodeToString(h[:])
		if capMode() { // managed mode: buffer the write; capFlush leases + PUTs it via a cap (no store creds)
			if blobCacheKnown(key) {
				return key // already in this store (restore-seeded or a prior flush) -> dedup, like the s3 path
			}
			if !capBufferPut("blob/"+key, data) {
				fail("cap-mode postBlob outside a capBegin/capFlush bracket: blob/" + key)
			}
			return key
		}
		if blobCacheKnown(key) {
			return key // confirmed present in THIS store by a prior push -> skip the network stat (the slow part)
		}
		if s3Exists("blob/" + key) {
			fmt.Fprintf(os.Stderr, "  dedup blob/%s (already in store)\n", key[:12])
		} else {
			s3Put("blob/"+key, data) // panics on persistent failure -> we never cache an unconfirmed chunk
		}
		blobCacheAdd(key) // reached only after a stat HIT or a successful PUT -> safe to remember
		return key
	}
	resp, err := http.Post(base+"/blob", "application/octet-stream", bytes.NewReader(data))
	check(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b))
}

func putRef(base, ref, val string) {
	if base == "disco" {
		discoPut("ref/"+ref, []byte(val))
		return
	}
	if isS3(base) {
		if capMode() { // managed mode: buffer the pointer write; capFlush leases + PUTs it via a cap
			if !capBufferPut("ref/"+ref, []byte(val)) {
				fail("cap-mode putRef outside a capBegin/capFlush bracket: ref/" + ref)
			}
			return
		}
		s3Put("ref/"+ref, []byte(val))
		return
	}
	req, _ := http.NewRequest("PUT", base+"/ref/"+ref, strings.NewReader(val))
	resp, err := http.DefaultClient.Do(req)
	check(err)
	resp.Body.Close()
}

// delRef makes a ref stop resolving by TOMBSTONING it -- overwriting the object with "" -- so getRef reads
// back "" -> a blank (no-resolve) login. We deliberately do NOT RemoveObject (hard-delete) the key:
//   1. It only needs PUT, which the store creds always have (RemoveObject can be denied by a write-scoped
//      key; that denial was silently dropped and the head survived -- the original "deleted but still logs
//      in" bug); and
//   2. on Filebase (IPFS-backed S3) a just-DELETED key's GET can still serve the stale pinned content for a
//      while, whereas a key that simply EXISTS pointing at an empty object reads back empty consistently.
//      So a hard delete after the tombstone would *reintroduce* the resolve-after-delete bug -- proven on
//      FP3: the head reappeared on the next login even though the post-delete verify read empty.
// The empty tombstone object is left in the bucket (negligible) rather than risk that stale-read window.
// The HTTP test store has no DELETE verb either, so it takes the same overwrite-to-empty path.
func delRef(base, ref string) {
	if isS3(base) {
		if capMode() { // managed mode: tombstone via a write cap (the ref has a live lease) -- no store creds
			u, err := newGatewayClient(os.Getenv("GATEWAY_URL")).writeCap("ref/" + ref)
			check(err)
			capPut(u, []byte{})
			return
		}
		s3Put("ref/"+ref, []byte{}) // tombstone to empty (strongly consistent; never hard-delete the key)
		return
	}
	putRef(base, ref, "")
}

// refExists reports whether a ref OBJECT is present in the store. This is what distinguishes a DELETE
// tombstone (delRef leaves an empty object that EXISTS) from a LOST / never-created head (the object is
// ABSENT). Both read back "" via getRef, so pushProfile uses this to recover a lost head WITHOUT
// resurrecting a deleted (tombstoned) profile. (#19, DIA-20260624-05.)
func refExists(base, ref string) bool {
	if isS3(base) {
		if capMode() { // managed mode: existence via a free read cap (no store creds)
			_, ok := capGet("ref/" + ref)
			return ok
		}
		return s3Exists("ref/" + ref)
	}
	if base == "disco" {
		_, ok := discoGet("ref/" + ref)
		return ok
	}
	resp, err := http.Get(base + "/ref/" + ref)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound
}

// deleteProfile drops a profile's head + recovery refs so (name,pass) no longer resolves. Returns false
// (deletes nothing) if the profile doesn't resolve -- unknown name OR wrong passphrase -- so a bad pass
// can't wipe a profile. Blobs are left for store GC. Shared by the `delete-profile` CLI verb and the login
// daemon's DELETE verb (the user-facing "delete my profile").
func deleteProfile(base, name, pass string) bool {
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return false
	}
	if v, isV := parseVault(getBlob(base, head)); isV && v.RecoveryRef != "" {
		delRef(base, v.RecoveryRef) // also kill the 12-word recovery path so the name is fully gone
	}
	delRef(base, profileRefV2(name, pass))     // v2 head
	delRef(base, profileRefLegacy(name, pass)) // legacy head (an un-migrated profile)
	// Verify the head actually stopped resolving -- so a store that refused the delete reports failure
	// (NOTFOUND -> "couldn't delete") instead of a false "deleted" that the profile survives.
	return getRef(base, headKey(base, name, pass)) == ""
}

// pushProfile seals srcDir into the profile's store ref (CDC chunks + manifest). A VAULT profile keeps its
// DK + keyslots (so a rotation/recovery isn't clobbered); a legacy/un-migrated profile seals under the
// derived key (== DK, so old chunks stay valid).
//
// CRITICAL: a seal must NEVER RESURRECT a deleted profile. A bare (gate) profile is always established by
// create-vault BEFORE any roam/seal, so finding head=="" here means it was DELETED -- re-creating the ref
// would un-tombstone it (this was the "deleted profile still logs in" bug: the live session's continuous
// syncLoop sealed the just-deleted profile and re-created its head). So for a bare name with no head, skip.
// The "name#de" / "name#media" data refs legitimately lazy-create on their FIRST seal (create-vault doesn't
// make them), so they're exempt -- and they don't gate login (only the bare CE ref does).
// manifestChunkCount fetches + unseals a CDC manifest blob and returns its chunk count (0 on any error, a
// missing head, or a non-CDC/legacy head). The shrink guard in pushProfile compares a new seal against the
// current head with this, so a partial/failed restore can't silently overwrite full data with a tiny set.
// loadCDCManifest fetches + unseals a CDC manifest blob and returns it. Empty manifest on any error, a
// missing head, or a non-CDC/legacy head (so callers can always read .Chunks/.Sizes safely). (#85 reuses it
// to carry per-chunk sizes forward; the shrink guard reads its chunk count.)
func loadCDCManifest(base string, dk []byte, manifestHash string) (m chunkManifest) {
	if manifestHash == "" {
		return chunkManifest{}
	}
	defer func() {
		if recover() != nil {
			m = chunkManifest{}
		}
	}()
	pt := unseal(dk, getBlob(base, manifestHash))
	if !bytes.HasPrefix(pt, cdcMagic) {
		return chunkManifest{}
	}
	if json.Unmarshal(pt[len(cdcMagic):], &m) != nil {
		return chunkManifest{}
	}
	return m
}

func manifestChunkCount(base string, dk []byte, manifestHash string) int {
	return len(loadCDCManifest(base, dk, manifestHash).Chunks)
}

func pushProfile(base, profile, pass, src string) {
	if capMode() { // managed mode: buffer all writes, then one batch lease + cap-PUT at the end (no creds)
		capBegin()
		defer capFlush()
	}
	t0 := time.Now() // seal duration -> the log, so a slow logoff is diagnosable (network vs local crypto)
	// #80: the CURRENT head key (v2 if migrated, else legacy) -- the READ location for this seal. putHead below
	// may migrate a legacy head to v2 mid-op, so headKey is only safe to sample here, for the read.
	ref := headKey(base, profile, pass)
	// #72 restore-receipt key: the STABLE v2 identity ref, NOT headKey. If this seal migrates the head (legacy->v2),
	// headKey flips, so keying the receipt by it would orphan the login-time receipt and refuse EVERY seal after
	// the migrating one (incl. the logoff seal -> lost session-end data). Login records the receipt under this
	// same profileRefV2, so the gate stays consistent across the migration and across sessions. (#80)
	rcptRef := profileRefV2(profile, pass)
	head := getRef(base, ref)
	if head == "" && !strings.Contains(profile, "#") {
		// head=="" is ambiguous. A DELETE tombstones the ref to an empty object that still EXISTS; a LOST
		// head (churn / corruption / partial state) leaves the ref ABSENT. NEVER resurrect a deleted profile
		// -- a tombstone present means the bare ref was explicitly deleted, so skip (this is the #12
		// read-after-delete guard, and a deleted profile has no live session anyway since it was reaped). But
		// a LOST head on a LIVE session (data present, valid creds) SHOULD recover: fall through to re-create
		// the head from the working set. Re-sealing an ABSENT ref can't resurrect a delete (deletes tombstone,
		// they don't vanish). (#19, DIA-20260624-05.)
		if refExists(base, ref) {
			fmt.Printf("push: profile %q tombstoned -> skip (not resurrecting a deleted profile)\n", profile)
			return
		}
		fmt.Printf("push: profile %q head ABSENT (lost, not deleted) -> re-sealing to recover\n", profile)
	}
	var dk []byte
	var v *vault
	if head != "" {
		if pv, isV := parseVault(getBlob(base, head)); isV {
			v = pv
			if dk = unwrapDK(v, "pass", deriveKey(profile, pass)); dk == nil {
				if dk = seDK(v, profile, pass); dk == nil { // hardened identity: seal via the secure-element slot
					fail("push: no slot opens (pass + secure-element both failed)")
				}
			}
		}
	}
	if dk == nil {
		dk = deriveKey(profile, pass)
	}
	if capMode() {
		capSetSession(profile, pass, dk) // fold the post-spend wallet into this seal's flush (free)
	}
	// Parallel seal + store-PUT (DIA-20260624-07): each chunk's seal + content-addressed PUT is independent, so
	// a bounded worker pool runs them concurrently -- saturating the link instead of one serial round-trip per
	// chunk. Results land by index so the manifest stays ordered; PUT is skip-if-exists so concurrency doesn't
	// change dedup; the ref is written only AFTER every chunk lands (below), so a mid-stream failure leaves the
	// previous head intact. The chunker below (incremental file-aligned walker, or the legacy whole-tar CDC)
	// feeds sealChunk/cachedChunk. (#69)
	compress := compressChunks()
	sem := make(chan struct{}, uploadWorkers())
	var pmu sync.Mutex
	var pwg sync.WaitGroup
	var perr error
	results := map[int]string{}
	sizeOf := map[string]int64{} // #85: sealed blob size per freshly-sealed chunk hash (guarded by pmu)
	nchunks := 0
	// Seal progress for the logoff "Saving your data…" bar (DIA-20260625-04): a ticker streams BYTES processed
	// (each chunk counts once sealed -- uploaded OR deduped) against the source size, via writeProg ->
	// NOWHERE_PROGRESS, which the daemon relays to LogoffActivity. No-op when the env isn't set (CLI/host). An
	// unchanged seal flies to 100% (all dedup); a fresh 175 MB advances as it uploads.
	var totalBytes int64
	filepath.Walk(src, func(_ string, fi os.FileInfo, e error) error {
		if e == nil && fi.Mode().IsRegular() {
			totalBytes += fi.Size()
		}
		return nil
	})
	var doneBytes int64
	if capMode() {
		// Managed mode buffers new blobs now and uploads them in the DEFERRED capFlush, so this scan/chunk/dedup
		// pass is genuinely the "prepare" phase -- label it so; capFlush flips to "save" for the upload. Direct
		// mode uploads inline here, so leave its label as the default "Saving…" (DIA-20260701-01).
		setProgPhase("prepare")
	}
	progStop := make(chan struct{})
	go func() {
		tk := time.NewTicker(400 * time.Millisecond)
		defer tk.Stop()
		for {
			writeProg(int(atomic.LoadInt64(&doneBytes)), int(totalBytes))
			select {
			case <-progStop:
				return
			case <-tk.C:
			}
		}
	}()
	cct := contentCacheTagFor(dk) // content-cache tag for THIS profile's DK (DIA-20260626-01)
	ver := 1
	if compress {
		ver = 2 // chunks are zstd-compressed-then-sealed; restore decompresses (DIA-20260624-08)
	}
	// sealChunk seals+uploads one plaintext chunk in the bounded worker pool, returning the manifest index it
	// was assigned; the hash lands in results[i] when the worker finishes.
	sealChunk := func(c []byte) int {
		i := nchunks
		nchunks++
		sem <- struct{}{}
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					pmu.Lock()
					if perr == nil {
						perr = fmt.Errorf("seal/upload chunk %d: %v", i, r)
					}
					pmu.Unlock()
				}
			}()
			// Content cache: an UNCHANGED chunk reuses its sealed form -- if its plaintext hash maps to a
			// sealed blob we've CONFIRMED is in the store (blobCacheKnown), skip the zstd+encrypt+sha256+post.
			// Else do the full seal and record the mapping. (DIA-20260626-01)
			ph := sha256.Sum256(c)
			phx := contentKey(ver, hex.EncodeToString(ph[:]))
			var h string
			var sealedLen int64
			if sealed := contentCacheGet(cct, phx); sealed != "" && blobCacheKnown(sealed) {
				h = sealed // confirmed-present sealed form -> skip the zstd+encrypt+sha256+post entirely
				atomic.AddInt64(&contentCacheHits, 1)
			} else {
				sealedBytes := seal(dk, packChunk(c, compress))
				sealedLen = int64(len(sealedBytes)) // #85: the stored blob size, recorded into the manifest
				h = postBlob(base, sealedBytes)
				contentCachePut(cct, phx, h)
			}
			pmu.Lock()
			results[i] = h
			if sealedLen > 0 {
				sizeOf[h] = sealedLen
			}
			pmu.Unlock()
			atomic.AddInt64(&doneBytes, int64(len(c)))
		}()
		return i
	}
	// cachedChunk places an already-sealed, known-present chunk hash directly -- an unchanged large file reused
	// from the scan cache, with no read, no crypto, and no upload (#69).
	cachedChunk := func(h string) int {
		i := nchunks
		nchunks++
		pmu.Lock()
		results[i] = h
		pmu.Unlock()
		atomic.AddInt64(&blobCacheHits, 1)
		atomic.AddInt64(&contentCacheHits, 1)
		return i
	}
	var ranges []scanRange
	var seen map[string]bool
	var sc *fileScanCache
	if incrementalSeal() {
		// Incremental (#69): walk files, chunk file-aligned, and skip re-reading unchanged large files via the
		// scan cache. A periodic full re-hash self-heals a content change that kept an identical mtime+size.
		sc = newFileScanCache(dk)
		forceFull := sealCounterNext(cct)
		ranges, seen = walkChunks(src, sc, sealFileThreshold(), forceFull,
			sealChunk, cachedChunk, func(n int64) { atomic.AddInt64(&doneBytes, n) })
	} else {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(tarDirTo(src, pw)) }()
		cdcSplit(pr, func(c []byte) { sealChunk(c) })
	}
	pwg.Wait()
	close(progStop)
	if perr != nil {
		fail(perr.Error())
	}
	chunks := make([]string, nchunks)
	for i := range chunks {
		chunks[i] = results[i]
	}
	// #85: read the OLD manifest once -- reused below for the shrink-guard chunk count AND here to carry each
	// unchanged chunk's size forward, so the footprint never sizes chunks with a per-chunk store round-trip.
	oldManifest := head
	if v != nil {
		oldManifest = v.Manifest
	}
	oldM := loadCDCManifest(base, dk, oldManifest)
	oldSizes := map[string]int64{}
	if len(oldM.Sizes) == len(oldM.Chunks) {
		for i, h := range oldM.Chunks {
			oldSizes[h] = oldM.Sizes[i]
		}
	}
	// Per-chunk sealed sizes for the manifest (#85): freshly-sealed chunks come from sizeOf; unchanged ones are
	// carried from the old manifest; a chunk in neither (a pre-#85 head being re-sealed for the first time) is
	// sized once via blobSize, then recorded in every future manifest -- so this network cost is paid at most once.
	sizes := make([]int64, nchunks)
	backfilled := 0
	for i, h := range chunks {
		if sz := sizeOf[h]; sz > 0 {
			sizes[i] = sz
		} else if sz := oldSizes[h]; sz > 0 {
			sizes[i] = sz
		} else {
			sizes[i] = blobSize(base, h) // one-time migration of a pre-#85 head (cheap via capSize ranged GET, #87)
			// The progress ticker has stopped, so a big one-time back-fill would be SILENT -> the daemon's stall
			// watchdog fires -> ERR-TIMEOUT (seen on a 3.1 GB profile). Emit progress every few probes so the
			// caller sees the migration advancing and the watchdog stays alive. (#87)
			if backfilled++; backfilled%16 == 0 {
				writeProg(i+1, nchunks)
			}
		}
	}
	mj, _ := json.Marshal(chunkManifest{Version: ver, Chunks: chunks, Sizes: sizes})
	manifestHash := postBlob(base, seal(dk, append(append([]byte{}, cdcMagic...), mj...)))
	// SHRINK GUARD (#72, DIA-20260630-41): refuse to overwrite a substantial head with a DRASTICALLY smaller
	// working set. A partial/failed restore, or a half-wiped session, sealing over full data is silent loss --
	// proven 2026-06-30 when a failed 46% restore's seal took profile b from 2313 chunks -> 187, destroying a
	// 2.8 GB map. If the new manifest has under 1/5 the chunks of the current head (head non-trivial), ABORT
	// and keep the old head (the freshly-uploaded chunks are simply left unreferenced -> GC'd). This is a
	// safety NET, not the full fix: a genuine large deletion trips it too, so NOWHERE_SEAL_FORCE=1 overrides,
	// and the precise complement (only a fully-restored session seals) is tracked in #72/#73.
	if os.Getenv("NOWHERE_SEAL_FORCE") != "1" {
		oldN := len(oldM.Chunks)
		// KEYSTONE (#72): once a ref holds REAL data in the store (oldN>0), refuse to overwrite it unless THIS
		// session holds a completion receipt for the ref -- proof it fully restored that head at login, or
		// previously sealed it this session (restore_receipt.go). A partial/failed restore (e.g. a media set
		// that only got 60% down) writes NO receipt, so its seal is refused HERE and the good head survives,
		// PER PHASE -- the precise complement of the shrink-count net below. An empty/new head (oldN==0, e.g. a
		// freshly created vault, or a legacy non-CDC head restored atomically) has nothing to protect, so it's
		// exempt and the first real seal proceeds (and records the receipt). Inert off-device (no state dir ->
		// restoreReceiptDir()==""), so host CLI / tests are unchanged.
		if oldN > 0 && restoreReceiptDir() != "" && !haveRestoreReceipt(rcptRef) {
			fmt.Printf("push: profile %q SEAL REFUSED -- no restore-completion receipt for a non-empty head (%d chunks); refusing to overwrite good data with a partial/incomplete session (set NOWHERE_SEAL_FORCE=1 to override)\n", profile, oldN)
			return
		}
		// Secondary net (defense-in-depth): refuse a DRASTIC shrink even WITH a receipt -- the "restored fine,
		// then a mid-session wipe" case the receipt can't see. A genuine large deletion trips it too -> FORCE.
		if oldN >= 20 && nchunks*5 < oldN {
			fmt.Printf("push: profile %q SEAL REFUSED -- new head %d chunks << current %d (shrink guard: likely a partial restore; set NOWHERE_SEAL_FORCE=1 to override)\n", profile, nchunks, oldN)
			return
		}
	}
	if v != nil {
		pushSnapshot(v, manifestHash, v.HeadKind) // #58: retain the OUTGOING head (with its kind) before repointing
		v.Manifest = manifestHash      // keep the keyslots; just repoint the data + both refs
		v.Version++                    // bump + re-sign so the new head is fresh + tamper-evident
		v.HeadKind = sealKind()        // #58: this new head's kind -- manual "Back up now" vs automatic sync/logoff
		v.LastSeal = time.Now().Unix() // "last active" for the welcome-back on the next login (DIA-20260625-05)
		signVault(v, dk)
		vh := postBlob(base, serializeVault(v))
		putHead(base, profile, pass, vh)
		if v.RecoveryRef != "" {
			putRef(base, v.RecoveryRef, vh)
		}
		bumpAnchor(profileRefV2(profile, pass), v.Version) // advance the rollback anchor (no-op unless enabled); #80: v2 ref
		writeRestoreReceipt(rcptRef, vh)                 // #72: we now hold a byte-complete copy of this head (keyed by stable v2 ref)
		fmt.Printf("push: profile %q (vault) -> %d chunks (%d net-skipped, %d crypto-skipped), gen %s v%d in %s\n",
			profile, len(chunks), atomic.LoadInt64(&blobCacheHits), atomic.LoadInt64(&contentCacheHits), short(vh), v.Version, time.Since(t0).Round(time.Millisecond))
	} else {
		putHead(base, profile, pass, manifestHash)
		writeRestoreReceipt(rcptRef, manifestHash) // #72: we now hold a byte-complete copy of this head (keyed by stable v2 ref)
		fmt.Printf("push: profile %q -> %d chunks (%d net-skipped, %d crypto-skipped), gen %s in %s\n",
			profile, len(chunks), atomic.LoadInt64(&blobCacheHits), atomic.LoadInt64(&contentCacheHits), short(manifestHash), time.Since(t0).Round(time.Millisecond))
	}
	// #69: the head is committed -> refresh the scan cache with the freshly-chunked large files (their hashes
	// are now referenced by the new head) and prune deleted files. Reached only on a successful, non-aborted
	// seal (the shrink guard returns above). A cap-mode capFlush failure after this leaves stale entries, but
	// sc.get re-verifies every cached chunk is blobCacheKnown next time, so a missing one self-corrects.
	if sc != nil {
		for _, r := range ranges {
			hs := make([]string, 0, r.end-r.start)
			for i := r.start; i < r.end; i++ {
				hs = append(hs, results[i])
			}
			sc.put(r.rel, r.mtime, r.size, r.mode, hs)
		}
		sc.save(seen)
	}
}

// uploadWorkers is the parallel seal+upload concurrency for pushProfile (NOWHERE_UPLOAD_WORKERS, default 8).
// It bounds both the concurrent store PUTs and the in-flight chunk RAM (workers x up to cdcMax). Higher
// saturates a high-latency link better; lower is gentler on phone RAM.
func uploadWorkers() int {
	if n, err := strconv.Atoi(os.Getenv("NOWHERE_UPLOAD_WORKERS")); err == nil && n > 0 {
		return n
	}
	return 8
}

// tarDirTo streams a tar of dir into w -- no whole-tree buffer (the CDC push pipes this through the
// chunker so memory stays bounded to one chunk even for a multi-GB /data/media). The per-entry bytes come
// from writeTarEntry, SHARED with the incremental chunker (incremental_seal.go) so the two seal paths produce
// byte-identical tars. Live-user churn (grow/shrink/vanish) is handled there.
func tarDirTo(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // a path vanished mid-walk under the live user -> skip, don't abort the whole seal
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		return writeTarEntry(tw, p, info, rel)
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// safeJoin joins name under dir and reports whether the result stays inside dir -- the guard against a
// path-traversal ("tar-slip") entry like "../../x". REQUIRED here because untar runs as ROOT via the su:s0
// workers (nowhere_roamd restoring /data/user/N, nowhere_otad restoring /data/ota_package), so an entry
// that escapes dir would be an arbitrary root file write; and the OS payload is sealed under a PUBLIC pass,
// so a hostile store could otherwise forge such an entry. (DIA-20260618-08, audit finding #1.)
func safeJoin(dir, name string) (string, bool) {
	dir = filepath.Clean(dir)
	target := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return target, true
}

// copyDropCache streams one tar entry src -> dst but periodically fsyncs + DROPS the written page cache
// (posix_fadvise DONTNEED) so a multi-GB restore file can't balloon the page cache into memory pressure --
// which killed lmkd and REBOOTED the FP3 mid-restore (#76). fadvise only drops CLEAN pages, so fsync first.
// Best-effort: a sync/fadvise error never fails the restore.
func copyDropCache(dst *os.File, src io.Reader) error {
	buf := make([]byte, 1<<20) // 1 MiB
	var written, lastDrop int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if written-lastDrop >= 64<<20 { // every ~64 MiB: flush + drop what's been written so far
				dst.Sync()
				unix.Fadvise(int(dst.Fd()), 0, written, unix.FADV_DONTNEED)
				lastDrop = written
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	dst.Sync()
	unix.Fadvise(int(dst.Fd()), 0, written, unix.FADV_DONTNEED)
	return nil
}

func untarFrom(dir string, r io.Reader) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		check(err)
		target, ok := safeJoin(dir, hdr.Name)
		if !ok { // tar-slip guard: reject an entry that would escape dir (see safeJoin)
			fmt.Fprintf(os.Stderr, "[restore] rejected unsafe tar entry %q\n", hdr.Name)
			continue
		}
		if hdr.FileInfo().IsDir() {
			os.MkdirAll(target, 0o700)
			continue
		}
		// Best-effort per file: a roamed-to user may not have every app the sealed data carries (a
		// not-yet-installed or absent package has no app-data dir to write into). Skip that file and keep
		// going rather than aborting the whole restore -- a slightly different app set must not blank the
		// login. The app-provisioning pass reinstalls the apps; their data re-seals on the next logout.
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "[restore] skip %q: mkdir parent: %v\n", hdr.Name, err)
			continue
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
		if err != nil {
			fmt.Fprintf(os.Stderr, "[restore] skip %q: %v\n", hdr.Name, err)
			continue
		}
		if err := copyDropCache(f, tr); err != nil { // #76: bounded page cache (fsync+fadvise) so a multi-GB restore doesn't OOM
			f.Close()
			fmt.Fprintf(os.Stderr, "[restore] skip %q: copy: %v\n", hdr.Name, err)
			continue
		}
		f.Close()
	}
}

// tarDir/untarTo: buffered wrappers kept for the legacy push-set / restore-set manifest paths.
func tarDir(dir string) []byte {
	var buf bytes.Buffer
	check(tarDirTo(dir, &buf))
	return buf.Bytes()
}
func untarTo(dir string, data []byte) { untarFrom(dir, bytes.NewReader(data)) }

// ---- content-defined chunking (CDC) for the roaming data path (push/restore) ----
// The worker streams a directory's tar through a gear-hash chunker (FastCDC-style): the tar is split
// at content-defined boundaries, so an edit only rewrites the chunks it touches and every unchanged
// chunk dedups in the store (seal() uses a convergent nonce -> identical plaintext seals to identical
// ciphertext -> identical content hash -> postBlob skips the upload). Memory is bounded to one chunk,
// not the whole tree -- the point of this slice (a multi-GB /data/media no longer tars into RAM).
const (
	cdcMin  = 256 * 1024        // no boundary below this (avoid tiny chunks)
	cdcMax  = 4 * 1024 * 1024   // forced boundary here (cap chunk size / RAM)
	cdcMask = (1 << 20) - 1     // ~1 MiB average chunk
)

var gearTable [256]uint64

func init() {
	// Deterministic gear table (fixed seed, splitmix64) so chunk boundaries are identical on every
	// build/device -> cross-version and cross-device dedup. Must never change once data is in a store.
	x := uint64(0x9E3779B97F4A7C15)
	for i := range gearTable {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		gearTable[i] = z ^ (z >> 31)
	}
}

// cdcSplit reads r and calls emit once per content-defined chunk (a fresh slice the callee owns).
func cdcSplit(r io.Reader, emit func([]byte)) {
	buf := make([]byte, 0, cdcMax)
	rd := make([]byte, 64*1024)
	var h uint64
	flush := func() {
		c := make([]byte, len(buf))
		copy(c, buf)
		emit(c)
		buf = buf[:0]
		h = 0
	}
	for {
		n, err := r.Read(rd)
		for i := 0; i < n; i++ {
			buf = append(buf, rd[i])
			h = (h << 1) + gearTable[rd[i]]
			if len(buf) >= cdcMin && (len(buf) >= cdcMax || (h&cdcMask) == 0) {
				flush()
			}
		}
		if err == io.EOF {
			break
		}
		check(err)
	}
	if len(buf) > 0 {
		flush()
	}
}

// chunkManifest is the ref target for a CDC push: the ordered chunk content-hashes. Sealed + prefixed
// with cdcMagic so restore can tell it apart from a legacy single-blob tar (which it still reads).
type chunkManifest struct {
	Version int      `json:"v"`
	Chunks  []string `json:"chunks"`
	// Sizes is the sealed BLOB size (bytes) of each chunk, parallel to Chunks (#85). It lets profileFootprint
	// (and billing's payRent/leaseRefs) size the profile from the manifest alone, instead of a per-chunk store
	// round-trip -- which in cap/managed mode DOWNLOADED every chunk just to measure it. omitempty keeps a
	// legacy (pre-#85) manifest byte-identical; a manifest missing Sizes falls back to the per-chunk lookup.
	Sizes []int64 `json:"sz,omitempty"`
	// Deferred is the on-demand-media index (#84): rel path -> the file's own chunk region + metadata, for files
	// classifyDeferred() tags as bulk media. These files are EXCLUDED from the login-critical restore (P2) and
	// served on-access from the store by the media daemon (P3), so login doesn't block on GBs of maps/photos.
	// omitempty keeps a pre-#84 manifest byte-identical; a manifest without it restores everything at login as
	// before. Populated in P2; defined here so the on-disk format is pinned. (The chunk hashes are content-
	// addressed, same as Chunks, so a deferred file's blobs live in the store like any other chunk.)
	Deferred map[string]deferredFile `json:"deferred,omitempty"`
}

// deferredFile is one on-demand-media file's self-contained record (#84): its ordered chunk hashes (a
// file-aligned CDC region, so it reconstructs standalone) plus the metadata needed to present it through the
// media-FUSE mount (size for getattr, mode + mtime to mirror the original).
type deferredFile struct {
	Chunks []string `json:"c"`
	Size   int64    `json:"s"`
	Mode   uint32   `json:"m"`
	MTime  int64    `json:"t"`
}

var cdcMagic = []byte("DSPRCDC1")

// ---- chunk cache: optional on-disk delta-download cache for CDC restore (DIA-20260618-02) ----
// CDC chunks are content-addressed (the manifest stores h = hex(sha256(sealed chunk))), so a chunk is
// immutable and reusable across versions: with convergent sealing an unchanged region of a v2->v3 OS
// payload (or a re-login's data) seals to identical ciphertext -> identical h. Caching the sealed blob by
// h lets a restore network-fetch ONLY the chunks it does not already have -- the P4.4b "true delta
// download" follow-on. The cache holds only CIPHERTEXT (like the store), so it is safe at rest. Enabled by
// NOWHERE_CHUNK_CACHE=<dir>; empty disables it (restore fetches every chunk, as before).
var chunkCacheDir = os.Getenv("NOWHERE_CHUNK_CACHE")
var chunkHits, chunkMiss int64 // atomic: cdcRestore fetches chunks concurrently (DIA-20260624-07)

// cacheRead returns the cached sealed blob for h iff present AND its hash verifies. Content-addressed, so a
// mismatch means corruption/truncation -> evict and miss, making the cache self-healing. Pure: no network.
func cacheRead(dir, h string) ([]byte, bool) {
	if dir == "" {
		return nil, false
	}
	p := filepath.Join(dir, h)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != h {
		os.Remove(p) // corrupt -> drop so the next fetch repopulates it
		return nil, false
	}
	return b, true
}

// cacheWrite stores a sealed blob under its content hash atomically (temp + rename) so an interrupted
// restore never leaves a partial file that would later read as corrupt. Best-effort: cache I/O errors
// never fail the restore (the chunk is already in hand).
func cacheWrite(dir, h string, b []byte) {
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	if _, err = tmp.Write(b); err != nil || tmp.Close() != nil {
		os.Remove(tmp.Name())
		return
	}
	os.Rename(tmp.Name(), filepath.Join(dir, h))
}

// getChunk is getBlob with the chunk cache: a hit skips the network entirely; a miss fetches and populates
// the cache. cdcRestore calls this from concurrent fetch goroutines, so the hit/miss counters are atomic.
//
// The STORE fetch is VERIFIED against the content-address hash (the local cache already self-verifies in
// cacheRead). A blob whose bytes don't hash to its key is corrupt in the store -- a torn PUT the store 200'd,
// or at-rest bit-rot -- and un-checked it reaches unseal only as a cryptic "cipher: message authentication
// failed", bricking the whole restore with a misleading "check your connection". We verify + RETRY a few
// times (a fresh presign may reach a healthy replica, so a TRANSIENT corruption self-heals and the login
// still succeeds); only a PERSISTENTLY corrupt blob fails, and it fails as an explicit integrity error so the
// cause is unambiguous in the log. Never cache unverified bytes. (#72 data-integrity.)
func getChunk(base, h string) []byte {
	if b, ok := cacheRead(chunkCacheDir, h); ok {
		atomic.AddInt64(&chunkHits, 1)
		return b
	}
	const tries = 3
	var b []byte
	for attempt := 1; ; attempt++ {
		b = getBlob(base, h)
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) == h {
			break
		}
		got := hex.EncodeToString(sum[:])
		if attempt >= tries {
			panic(fmt.Sprintf("corrupt chunk in store: blob/%s is %d bytes hashing to %s -- your saved data may be damaged", h, len(b), got))
		}
		fmt.Fprintf(os.Stderr, "  chunk %s corrupt in store (got %s), refetch %d/%d\n", h[:12], got[:12], attempt, tries)
		if capMode() {
			capInvalidate("blob/" + h) // drop the cached cap so the retry re-presigns (maybe a healthy edge/replica)
		}
		time.Sleep(s3RetryBase << uint(attempt-1))
	}
	atomic.AddInt64(&chunkMiss, 1)
	cacheWrite(chunkCacheDir, h, b)
	return b
}

// downloadWorkers is the parallel chunk-fetch concurrency for cdcRestore (NOWHERE_DOWNLOAD_WORKERS). In
// managed mode each chunk costs an EU gateway presign round-trip BEFORE its store GET, so the restore is
// latency-bound, not bandwidth-bound -- more in-flight fetches hide that latency and let the GETs saturate the
// link. Bumped 8 -> 24 (#71 step 1; paired with the pooled keep-alive transport in init() so the extra workers
// reuse connections instead of churning TLS). Each worker holds up to one cdcMax (4 MiB) chunk -> ~96 MiB cap.
func downloadWorkers() int {
	if n, err := strconv.Atoi(os.Getenv("NOWHERE_DOWNLOAD_WORKERS")); err == nil && n > 0 {
		return n
	}
	return 24
}

// The stdlib http.DefaultTransport keeps only MaxIdleConnsPerHost=2 idle keep-alive connections, so under the
// restore's high fetch concurrency the extra workers can't reuse connections and pay a fresh TLS handshake to
// the EU gateway (and the store) on nearly every chunk. Pool enough to cover downloadWorkers per host, for
// BOTH the gateway presigns (g.hc) and the store GETs (http.Get) -- both ride DefaultTransport. (#71 step 1.)
func init() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.MaxIdleConns = 256
		t.MaxIdleConnsPerHost = 64
	}
}

// cdcRestore streams a CDC chunk-manifest into an untar at dest (DIA-20260624-07). Chunks are fetched +
// unsealed CONCURRENTLY (a bounded window of downloadWorkers()) but WRITTEN strictly in order, so the tar
// reconstructs byte-identically -- the serial path before this paid ~1 round-trip per chunk (RTT-bound on a
// 10k-chunk restore). The writer releases the window only after consuming each chunk, so at most
// downloadWorkers() chunks are in flight (bounded RAM). A fetch failure (panic in getChunk/unseal) is
// recovered, aborts the rest via `done`, and surfaces as a pipe error -> untarFrom's check() -> fail-hard,
// exactly like the old serial loop. getChunk is cache-aware (delta download when NOWHERE_CHUNK_CACHE is set).
func cdcRestore(base string, key []byte, m chunkManifest, dest string) {
	total := len(m.Chunks)
	// Seed the known-present blob cache from this manifest: every chunk here is referenced by the live ref, so
	// it IS in the store -- recording that at login means the session's seals (incl. a quick logoff) skip the
	// per-chunk network stat from the start, instead of waiting for the first full sync to warm it. (DIA-20260625-08)
	for _, h := range m.Chunks {
		blobCacheAdd(h)
	}
	// #71 step 2: BATCH-prefetch every chunk's read-cap now, so the per-chunk fetch below hits capReadCache
	// instead of a gateway round-trip each (~2300 round-trips -> ~a handful). Best-effort: falls back to
	// per-chunk on any error or an old gateway. No-op off cap mode.
	if capMode() {
		keys := make([]string, len(m.Chunks))
		for i, h := range m.Chunks {
			keys[i] = "blob/" + h
		}
		prefetchReadCaps(keys)
	}
	cct := contentCacheTagFor(key) // seed the content cache too (plaintext->sealed), filled as chunks decrypt below
	writeProg(0, total)
	pr, pw := io.Pipe()
	type result struct {
		data []byte
		err  error
	}
	futures := make([]chan result, total)
	for i := range futures {
		futures[i] = make(chan result, 1)
	}
	sem := make(chan struct{}, downloadWorkers())
	done := make(chan struct{})
	// Launcher: kick off each chunk's fetch, at most downloadWorkers() ahead of the writer (which frees sem).
	go func() {
		for i, h := range m.Chunks {
			select {
			case sem <- struct{}{}:
			case <-done:
				return
			}
			go func(i int, h string) {
				var res result
				func() {
					defer func() {
						if r := recover(); r != nil {
							res = result{err: fmt.Errorf("fetch chunk %d: %v", i, r)}
						}
					}()
					b := unseal(key, getChunk(base, h))
					b, derr := unpackChunk(b, m.Version) // V>=2 -> zstd-decompress (DIA-20260624-08)
					if derr != nil {
						res = result{err: fmt.Errorf("decompress chunk %d: %v", i, derr)}
						return
					}
					res.data = b
					// Seed the content cache: this chunk's plaintext (b) -> its sealed hash (h), keyed by the
					// manifest version (the sealed encoding is version-specific), so the session's later seals
					// reuse the sealed form for unchanged chunks (warm from login). (DIA-20260626-01)
					ph := sha256.Sum256(b)
					contentCachePut(cct, contentKey(m.Version, hex.EncodeToString(ph[:])), h)
				}()
				select {
				case futures[i] <- res:
				case <-done:
				}
			}(i, h)
		}
	}()
	// Ordered writer: consume the futures in order, free the window, pipe into the untar.
	go func() {
		var e error
		for i := 0; i < total; i++ {
			res := <-futures[i]
			<-sem // release the window: the launcher may start one more fetch
			if res.err != nil {
				e = res.err
				break
			}
			if _, e = pw.Write(res.data); e != nil {
				break
			}
			writeProg(i+1, total)
		}
		close(done)
		pw.CloseWithError(e)
	}()
	untarFrom(dest, pr)
}

// ---- OTA version marker (DIA-20260618-03 self-service OTA) ----
// The latest published OS version lives in a small plain store ref (the OS is public; the version is not
// secret). `ota-mark` sets it on publish; `ota-check` compares it to the running /system stamp so the device
// knows whether a newer OS exists WITHOUT downloading the (large) payload first.
//
// The ref is PER-EDITION: the FP3 (LineageOS) and Pixel (Endospore/GrapheneOS) images are not
// interchangeable, so each edition publishes under its own version ref. The agent is one shared binary, so
// the ref is resolved at runtime, in order:
//  1. $NOWHERE_OTA_VERSION_REF -- the explicit override the build-host publish sets (no os-ref file there);
//  2. derived from the baked per-edition /system/etc/nowhere/os-ref ("os-<dev>" -> "ota/<dev>/version"),
//     so on-device callers (otad's ota-check AND the daemon's OTA-STATUS probe) both get the right ref with
//     no per-caller env plumbing -- os-fp3 -> ota/fp3/version, os-lynx -> ota/lynx/version;
//  3. the FP3 ref, so the shipping FP3 (which bakes no os-ref) is unchanged.
func otaVersionRef() string {
	if r := strings.TrimSpace(os.Getenv("NOWHERE_OTA_VERSION_REF")); r != "" {
		return r
	}
	if b, err := os.ReadFile("/system/etc/nowhere/os-ref"); err == nil {
		if dev := strings.TrimPrefix(strings.TrimSpace(string(b)), "os-"); dev != "" {
			return "ota/" + dev + "/version"
		}
	}
	return "ota/fp3/version"
}

// semverLess reports whether dotted-numeric version a is strictly older than b ("0.1.0" < "0.2.0"). Missing
// fields zero-pad; a non-numeric field counts as 0 (best-effort, never panics).
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.TrimSpace(pa[i]))
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.TrimSpace(pb[i]))
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// otaCheck reports the latest published OS version (store ref) and whether it is newer than the running
// /system stamp. Shared by the `ota-check` CLI verb and the daemon's OTA-STATUS probe (the gate "is an
// update available?" query that drives the user-confirm install prompt).
func otaCheck(base, runFile string) (latest string, avail bool) {
	if runFile == "" {
		runFile = "/system/etc/nowhere-ota-version"
	}
	running := "0.0.0"
	if b, err := os.ReadFile(runFile); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			running = s
		}
	}
	latest = getRef(base, otaVersionRef())
	return latest, latest != "" && semverLess(running, latest)
}

// restoreSet restores a profile's working set (items with prio <= maxPrio) into dest, returning the
// number of items restored. Shared by the `restore-set` CLI command and the login daemon. On any
// failure (wrong passphrase, unknown profile, missing blob) the helpers panic; callers recover and
// treat that as a blank result (blind login).
func restoreSet(base, profile, pass, dest string, maxPrio int, verbose bool) (int, bool) {
	key := deriveKey(profile, pass)
	head := getRef(base, headKey(base, profile, pass))
	if head == "" {
		if verbose {
			fmt.Printf("restore-set: profile %q empty -> nothing\n", profile)
		}
		return 0, false // ref didn't resolve: unknown profile or wrong passphrase -> invalid
	}
	var m Manifest
	check(json.Unmarshal(unseal(key, getBlob(base, head)), &m))
	sort.Slice(m.Items, func(i, j int) bool { return m.Items[i].Prio < m.Items[j].Prio })
	n := 0
	for _, it := range m.Items {
		if it.Prio > maxPrio {
			continue
		}
		itemDir, ok := safeJoin(dest, it.Name)
		if !ok { // tar-slip guard on the per-item subdir too (the manifest comes from the untrusted store)
			fmt.Fprintf(os.Stderr, "[restore] rejected unsafe item name %q\n", it.Name)
			continue
		}
		untarTo(itemDir, unseal(key, getBlob(base, it.Ref)))
		n++
		if verbose {
			fmt.Printf("  restored %q (prio=%d, %d B)\n", it.Name, it.Prio, it.Size)
		}
	}
	if verbose {
		fmt.Printf("restore-set: profile %q restored %d/%d items (maxPrio=%d)\n", profile, n, len(m.Items), maxPrio)
	}
	return n, true // ref resolved + manifest unsealed: valid login (even if n == 0)
}

// pushSet seals each subdir of root as an item, builds the manifest, and publishes the head ref for
// (profile, pass). Returns the item count. Shared by the `push-set` CLI and the daemon sync/logoff.
func pushSet(base, profile, pass, root string) int {
	key := deriveKey(profile, pass)
	entries, err := os.ReadDir(root)
	check(err)
	var m Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw := tarDir(filepath.Join(root, e.Name()))
		ref := postBlob(base, seal(key, raw))
		m.Items = append(m.Items, Item{Name: e.Name(), Prio: itemPrio(e.Name()), Ref: ref, Size: len(raw)})
	}
	mj, _ := json.Marshal(m)
	putHead(base, profile, pass, postBlob(base, seal(key, mj)))
	return len(m.Items)
}

// loginDaemon is the root side of the blind-login chooser. init creates an AF_UNIX socket
// (`socket nowhere_login` in nowhere.rc) and passes its fd via env ANDROID_SOCKET_nowhere_login.
// The chooser app connects, sends "<name>\n<pass>\n", and we restore that profile's working set into
// NOWHERE_STATE (default /data/nowhere/state, the tmpfs). The passphrase only ever lives in this
// process's memory -- never on disk, never in any argv. Reply: "OK <items>\n" or "BLANK\n" (uniform
// blank on wrong pass / unknown profile -> blind login / plausible deniability).
// pinnedChooserApp is the appId (uid modulo the 100000 per-user offset) of the first socket peer the daemon
// sees -- trust-on-first-use. The device boots straight into the kiosk gate (the only app at boot; no roamed
// user exists yet), so the first peer IS the chooser; we pin it and reject any later peer with a different
// appId. Defense-in-depth (audit #2): SELinux's connectto restriction is the primary gate, this is a belt
// for an ever-permissive build, and it needs no privileged read (SO_PEERCRED is just getsockopt). -1 = unpinned.
var pinnedChooserApp = -1

func loginDaemon() {
	dest := os.Getenv("NOWHERE_STATE")
	if dest == "" {
		dest = "/data/nowhere/state"
	}
	fd, err := strconv.Atoi(os.Getenv("ANDROID_SOCKET_nowhere_login"))
	if err != nil {
		fail("login-daemon: no ANDROID_SOCKET_nowhere_login fd from init")
	}
	// init already created + bound + listen()ed this socket; accept on the raw fd with a BLOCKING
	// loop. (net.FileListener's runtime poller doesn't reliably drive Accept on an inherited init
	// socket here -> it busy-spins and connections get refused.)
	syscall.SetNonblock(fd, false)
	syscall.Listen(fd, 8) // init creates + binds the socket but does NOT listen(); the daemon must.
	fmt.Fprintf(os.Stderr, "[logind] listening (fd %d) dest %s\n", fd, dest)
	go syncLoop(dest) // continuous background seal of the live roamed user (data safety on unclean power-off)
	for {
		nfd, _, aerr := syscall.Accept(fd)
		if aerr != nil {
			if aerr == syscall.EINTR {
				continue
			}
			fmt.Fprintf(os.Stderr, "[logind] accept err: %v\n", aerr)
			time.Sleep(time.Second)
			continue
		}
		// Defense-in-depth (DIA-20260618-08, audit #2): SELinux is the primary gate (only the nowhere_chooser
		// domain may connectto this socket), but the socket is world-rw, so also verify the peer APP via
		// SO_PEERCRED + trust-on-first-use (see pinnedChooserApp). The first peer at boot is the kiosk gate; pin
		// its appId and reject any later peer with a different appId (a roamed app would be a different package).
		// Non-root peers only; getsockopt(SO_PEERCRED) is just getopt on the socket (already allowed, no cap).
		if uc, e := syscall.GetsockoptUcred(nfd, syscall.SOL_SOCKET, syscall.SO_PEERCRED); e == nil && uc.Uid != 0 {
			app := int(uc.Uid) % 100000
			if pinnedChooserApp < 0 {
				pinnedChooserApp = app
				fmt.Fprintf(os.Stderr, "[logind] pinned chooser appId %d (peer uid %d)\n", app, uc.Uid)
			} else if app != pinnedChooserApp {
				fmt.Fprintf(os.Stderr, "[logind] reject peer uid %d (appId %d != chooser %d)\n", uc.Uid, app, pinnedChooserApp)
				syscall.Close(nfd)
				continue
			}
		}
		f := os.NewFile(uintptr(nfd), "nowhere_login_conn")
		conn, cerr := net.FileConn(f)
		f.Close()
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "[logind] FileConn err: %v\n", cerr)
			continue
		}
		handleLogin(conn, dest) // one login at a time -> no concurrent writers to the state dir
	}
}

func handleLogin(conn net.Conn, dest string) {
	defer conn.Close()
	defer func() { recover() }() // a malformed request must never kill the daemon
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	r := bufio.NewReader(conn)
	name, _ := r.ReadString('\n')
	name = strings.TrimRight(name, "\r\n")
	if name == "STATUS" {
		// Session probe for the home-screen gate: active iff a roam session is recorded. Keyed off the
		// .roamsession marker -- NOT "any file in the state dir": a reboot logoff wiped the tmpfs, but the
		// no-reboot logoff clears the marker without wiping, so leftovers (e.g. apps.out) must not read as
		// an active session (else a relaunched gate finishes itself, revealing the launcher).
		if _, err := os.Stat(filepath.Join(dest, ".roamsession")); err == nil {
			fmt.Fprint(conn, "ACTIVE\n")
		} else {
			fmt.Fprint(conn, "NONE\n")
		}
		return
	}
	if name == "GET-STORE" {
		handleGetStore(conn) // report the device's store config (endpoint/region/bucket; NEVER the secret key)
		return
	}
	if name == "SET-STORE" {
		// "SET-STORE\n<endpoint>\n<region>\n<bucket>\n<accesskey>\n<secretkey>\n" -> write nowhere.conf + reload.
		ep, _ := r.ReadString('\n')
		rg, _ := r.ReadString('\n')
		bk, _ := r.ReadString('\n')
		ak, _ := r.ReadString('\n')
		sk, _ := r.ReadString('\n')
		handleSetStore(conn, strings.TrimRight(ep, "\r\n"), strings.TrimRight(rg, "\r\n"),
			strings.TrimRight(bk, "\r\n"), strings.TrimRight(ak, "\r\n"), strings.TrimRight(sk, "\r\n"))
		return
	}
	if name == "TEST-STORE" {
		// "TEST-STORE\n<endpoint>\n<region>\n<bucket>\n<accesskey>\n<secretkey>\n" -> validate WITHOUT saving.
		ep, _ := r.ReadString('\n')
		rg, _ := r.ReadString('\n')
		bk, _ := r.ReadString('\n')
		ak, _ := r.ReadString('\n')
		sk, _ := r.ReadString('\n')
		handleTestStore(conn, strings.TrimRight(ep, "\r\n"), strings.TrimRight(rg, "\r\n"),
			strings.TrimRight(bk, "\r\n"), strings.TrimRight(ak, "\r\n"), strings.TrimRight(sk, "\r\n"))
		return
	}
	if name == "PING-STORE" {
		// "PING-STORE\n" -> reachability check of the ALREADY-CONFIGURED store, using the daemon's loaded
		// S3_* creds (no client-supplied secret -- unlike TEST-STORE). OK | ERR-NOSTORE | ERR-NET | ERR-AUTH
		// | ERR-BUCKET. Backs the store screen's live "Connected to <store>" banner.
		handlePingStore(conn)
		return
	}
	if name == "CREATE" {
		// "CREATE\n<name>\n<pass>\n" -> enroll a brand-new identity (Day-0).
		cname, _ := r.ReadString('\n')
		cname = strings.TrimRight(cname, "\r\n")
		cpass, _ := r.ReadString('\n')
		cpass = strings.TrimRight(cpass, "\r\n")
		handleCreate(conn, dest, cname, cpass)
		return
	}
	if name == "ENROLL-SE" {
		// "ENROLL-SE\n<name>\n<pass>\n<se_secret_hex>\n" -> harden the identity to THIS device's secure element
		// (Endospore E.3b): add the device `se` keyslot + drop the pass-only slot. The chooser computes
		// se_secret from a StrongBox key and calls this after a normal login/create. Reply OK | ERR-...
		en, _ := r.ReadString('\n')
		ep, _ := r.ReadString('\n')
		es, _ := r.ReadString('\n')
		handleEnrollSE(conn, strings.TrimRight(en, "\r\n"), strings.TrimRight(ep, "\r\n"), strings.TrimRight(es, "\r\n"))
		return
	}
	if name == "LOGOFF" {
		handleLogoff(conn, dest) // final sync of the live session, then wipe back to the gate
		return
	}
	if name == "ROAM-IN" {
		// Arc 2: "ROAM-IN\n<name>\n<pass>\n<uid>\n". The chooser has already created+started ephemeral
		// Android user <uid> (via DPM); we drive the su:s0 worker to restore that profile into
		// /data/user/<uid>, then the chooser switches into it. We bridge because the worker is su:s0 (an
		// app can't spawn it) and roam.req is root-only (an app can't write it).
		rname, _ := r.ReadString('\n')
		rpass, _ := r.ReadString('\n')
		ruid, _ := r.ReadString('\n')
		rsec, _ := r.ReadString('\n') // 4th line: se_secret (hex) for a hardened Endospore identity; empty otherwise
		handleRoamIn(conn, dest, strings.TrimRight(rname, "\r\n"), strings.TrimRight(rpass, "\r\n"), strings.TrimRight(ruid, "\r\n"), strings.TrimRight(rsec, "\r\n"))
		return
	}
	if name == "ROAM-OUT" {
		handleRoamOut(conn, dest) // seal the live user's data, then REBOOT to wipe+gate ("secure logoff")
		return
	}
	if name == "LOGOUT" {
		handleLogout(conn, dest) // reboot-free: seal, then queue the in-place reap for the user-0 chooser to do
		return
	}
	if name == "BACKUP" {
		handleBackup(conn, dest) // "Back up now": seal the live session to the store in place (no reap, no reboot)
		return
	}
	if name == "EXPORT" {
		// "EXPORT\n<bytelen>\n<bytelen raw bytes>" -> seal the export bundle (a zip of the session's vCard/iCal,
		// built by the chooser) under the profile key and store it at ref export/<name> for web retrieval
		// (DIA-20260628-09 P2b). The body is RAW after the length line, so it must be read HERE, where the
		// bufio.Reader lives.
		lenLine, _ := r.ReadString('\n')
		blen, lerr := strconv.Atoi(strings.TrimRight(lenLine, "\r\n"))
		if lerr != nil || blen < 0 || blen > 64*1024*1024 {
			fmt.Fprint(conn, "ERR-BADLEN\n")
			return
		}
		conn.SetDeadline(time.Now().Add(120 * time.Second)) // body read + DK derive + store PUT
		bundle := make([]byte, blen)
		if _, rerr := io.ReadFull(r, bundle); rerr != nil {
			fmt.Fprint(conn, "ERR-READ\n")
			return
		}
		handleExport(conn, dest, bundle)
		return
	}
	if name == "COLD-LOCK" {
		// P3 (DIA-20260625-13): cold-lock the live roamed session -- switch to the gate + STOP (not remove) the
		// user so its CE key is evicted (FBE-locked /data, resumable), keeping the data on the device. No seal
		// (the periodic sync already backed it up); fast. The gate offers RESUME via the .coldlock marker.
		handleColdLock(conn, dest)
		return
	}
	if name == "DROP-CACHES" {
		// P3: after the chooser stop-locks a user, flush the kernel dentry/inode cache so even a POWERED-ON
		// locked device is immediately ciphertext (a stopped user's CE key is gone, but already-cached inodes
		// stay readable from RAM until dropped; power-off clears it anyway). Root-only -> the su:s0 worker.
		triggerRoamWorker(dest, "dropcaches", "x", "", "", "", nil) // name="x" non-empty for the worker arg check
		fmt.Fprint(conn, "OK\n")
		return
	}
	if name == "CLEAN-STORAGE" {
		// "CLEAN-STORAGE\n<uid>\n" -> the chooser removed roamed user <uid>; removeUser leaves /data/system_ce/<uid>
		// (+ misc_ce) behind on this FBE build, so a reused user-id would inherit a stale, key-mismatched CE and
		// crash system_server on unlock. Root-only -> the su:s0 worker rm's the orphaned per-uid dirs (it hard-
		// guards uid >= 10, never user 0). (DIA-20260625-13)
		u, _ := r.ReadString('\n')
		triggerRoamWorker(dest, "cleanstorage", "x", "", strings.TrimRight(u, "\r\n"), "", nil)
		fmt.Fprint(conn, "OK\n")
		return
	}
	if name == "GET-COLDLOCK" {
		// P4 (DIA-20260625-13): the gate asks "is there a cold-locked session to resume?" Reply
		// "COLDLOCK <uid> <name>" (uid first so a name with spaces survives) or "NONE". The pass is NOT
		// returned -- there is none at rest; RESUME re-derives it from the freshly typed passphrase.
		handleGetColdLock(conn, dest)
		return
	}
	if name == "CLEAR-COLDLOCK" {
		// #3 (DIA-20260626-03): the gate's 12 h hard-wipe removed an un-resumed cold-locked user, so drop the
		// .coldlock marker -- otherwise GET-COLDLOCK would keep offering "Welcome back" for a user that's gone.
		// (RESUME clears it on the happy path; this is the wipe path's equivalent.) The user removal itself is
		// the device-owner chooser's job; this only forgets the marker.
		os.Remove(filepath.Join(dest, ".coldlock"))
		clearLocalSession(dest) // #4: a wiped cold-locked throwaway ends here too (tmpfs would clear it on power-off anyway)
		fmt.Fprint(conn, "OK\n")
		return
	}
	if name == "GET-SESSION-TYPE" {
		// #4 (DIA-20260626-04): the Profile screen asks whether the live session is a throwaway -> "LOCAL"
		// (offer "Save to your store") or "STORED" (already roams) or "NONE" (no live session).
		if _, err := os.Stat(filepath.Join(dest, ".roamsession")); err != nil {
			fmt.Fprint(conn, "NONE\n")
		} else if isLocalSession(dest) {
			fmt.Fprint(conn, "LOCAL\n")
		} else {
			fmt.Fprint(conn, "STORED\n")
		}
		return
	}
	if name == "GET-BILLING" {
		// The Profile "Storage & subscription" view asks for the live session's credit + lease state.
		// Read from the LOCAL wallet only (no passphrase, no gateway) -> "OK credit=.. through=.. epochsec=.."
		// or "NONE". Fast + local; GB-used (footprint) is the separate GET-USAGE below.
		fmt.Fprint(conn, billingLine(capWalletPath()))
		return
	}
	if name == "CLAIM" {
		// "CLAIM\n<code>\n" -- "Add credits": redeem a paid claim code into the live session's roaming wallet
		// (zero-knowledge blind tokens). Drains the WHOLE claim (the device doesn't know the purchased count).
		// Reply "OK <n>" (tokens added) | "NONE" (no session) | "LOCAL" (throwaway) | "ERR-NOCODE" | "ERR".
		c, _ := r.ReadString('\n')
		handleClaim(conn, dest, strings.TrimRight(c, "\r\n"))
		return
	}
	if name == "SUBSCRIBE" {
		// "SUBSCRIBE\n<subkey>\n" -- store a subscription secret + do an immediate first refill (blind-voucher
		// model, P3). Reply "OK <n>" (tokens added this epoch; 0 = stored but this epoch not yet credited by
		// the storefront) | "NONE" (no session) | "LOCAL" (throwaway) | "ERR-NOCODE" | "ERR".
		c, _ := r.ReadString('\n')
		handleSubscribe(conn, dest, strings.TrimRight(c, "\r\n"))
		return
	}
	if name == "GET-USAGE" {
		// The Profile "Storage & subscription" view's GB-USED line: the live profile's footprint in the
		// store. This is a STORE ROUND-TRIP (lists the profile's refs + sizes), so the screen loads it
		// async, separate from the fast local GET-BILLING. "NONE" (no session) | "LOCAL" (throwaway, not
		// in the store) | "OK bytes=N".
		handleGetUsage(conn, dest)
		return
	}
	if name == "PROMOTE" {
		// #4 (DIA-20260626-04): "Save to your store" -- turn the live throwaway into a real roaming profile:
		// create its store vault (recovery code) + seal the live data + drop the .localsession marker so it
		// roams from now on. Uses the session's own (name,pass). Reply "OK RECOVERY <12 words>" | ERR-...
		handlePromote(conn, dest)
		return
	}
	if name == "RESUME" {
		// "RESUME\n<pass>\n" -> verify the typed passphrase against the cold-locked user's CE (su:s0 worker
		// decrypts FBE in place); on success re-arm .roamsession + drop .coldlock and reply "OK <uid>" so the
		// chooser switches into the now-unlocked user. Wrong pass -> "WRONGPASS" (the CE stays cold).
		rp, _ := r.ReadString('\n')
		handleResume(conn, dest, strings.TrimRight(rp, "\r\n"))
		return
	}
	if name == "DELETE" {
		// "DELETE\n<name>\n<pass>\n" -> user-facing "delete my profile": drop it from the store (the typed
		// pass re-authenticates), then wipe the local session WITHOUT sealing.
		dname, _ := r.ReadString('\n')
		dpass, _ := r.ReadString('\n')
		handleDelete(conn, dest, strings.TrimRight(dname, "\r\n"), strings.TrimRight(dpass, "\r\n"))
		return
	}
	if name == "GET-SNAPSHOTS" {
		// #58: the Profile "Restore a snapshot" screen asks for the live session's retained rollback snapshots.
		handleGetSnapshots(conn, dest)
		return
	}
	if name == "ROLLBACK" {
		// "ROLLBACK\n<version>\n" -> #58: roll the live session's STORE head back to a retained snapshot, then
		// reap WITHOUT sealing (discarding the current local data -- else the logoff would re-seal it back over
		// the snapshot). The user signs in again and lands on the rolled-back (older good) head. version 0 = newest.
		vl, _ := r.ReadString('\n')
		handleRollback(conn, dest, strings.TrimRight(vl, "\r\n"))
		return
	}
	if name == "ROLLBACK-GATE" {
		// "ROLLBACK-GATE\n<name>\n<pass>\n" -> #58 P4: at the GATE, after a login restore failed (e.g. a corrupt
		// chunk), roll THIS profile's store head to its newest good snapshot so a RETRY login loads the last good
		// version. No live session to reap (login failed). Reply "OK <ver>" | NOSNAP | ERR.
		gn, _ := r.ReadString('\n')
		gp, _ := r.ReadString('\n')
		handleRollbackGate(conn, strings.TrimRight(gn, "\r\n"), strings.TrimRight(gp, "\r\n"))
		return
	}
	if name == "POLL-REAP" {
		// The user-0 chooser polls this; hand back the next no-reboot-logoff step (SWITCH then REAP, once each)
		// so it drives the teardown from its device-owner context (the worker can't -- device-owner wall).
		fmt.Fprintf(conn, "%s\n", nextReapAction())
		return
	}
	if name == "SEAL-STATUS" {
		// The logging-off SESSION polls this to render "Saving your data… N%" while the background seal runs,
		// then "DONE" once it lands (the switch+reap follow). Keeps the user in the session -- not the gate --
		// during the upload. (DIA-20260625-04)
		pendingReapMu.Lock()
		inFlight := sealInFlight
		failed := sealFail
		p := sealProg
		pendingReapMu.Unlock()
		if failed {
			fmt.Fprint(conn, "FAILED\n") // #86: seal failed -> session un-sticks (kept signed in, not reaped)
		} else if inFlight {
			fmt.Fprintf(conn, "SEALING %s\n", p)
		} else {
			fmt.Fprint(conn, "DONE\n")
		}
		return
	}
	if name == "GET-APPS" {
		handleGetApps(conn, dest) // hand back the roamed app list (worker surfaced it to apps.out)
		return
	}
	if name == "GET-PREFS" {
		handleGetPrefs(conn, dest) // hand back the roamed tz/locale (worker surfaced it to prefs.out)
		return
	}
	if name == "GRANT-PERMS" {
		// "GRANT-PERMS\n<uid>\n" -> re-grant the roamed runtime permissions (location, etc.) for user <uid>.
		// The chooser calls this AFTER install-existing (a perm needs an installed app); the worker captured the
		// grants at the last logout (they live in misc_de, outside the sealed dirs, so they don't roam with the
		// app data) and pm-grants them now -- without this every roamed login re-prompts. (DIA-20260625-06)
		u, _ := r.ReadString('\n')
		handleGrantPerms(conn, dest, strings.TrimRight(u, "\r\n"))
		return
	}
	if name == "SET-TZ" {
		// "SET-TZ\n<olson-or-empty>\n" -> apply a timezone LIVE (the chooser's in-session picker; empty = Automatic).
		z, _ := r.ReadString('\n')
		handleSetTz(conn, dest, strings.TrimRight(z, "\r\n"))
		return
	}
	if name == "RESOLVE-IP-TZ" {
		// Tier-4 IP fallback (docs/timezone-model.md): best-effort coarse zone from the public IP, used by the
		// chooser ONLY as the AUTOMATIC seed when there's no override + no NITZ/geo signal. Replies "tz=<zone>".
		handleResolveIpTz(conn)
		return
	}
	if name == "OTA-STATUS" {
		// Self-service OTA (DIA-20260618-03): is a newer OS published? Reply "AVAIL <ver>" or "NONE" so the
		// gate can offer a user-confirmed install. Checked in-process (the daemon IS the agent) -- no payload
		// download, just the version ref vs the /system stamp.
		if latest, avail := otaCheck("s3", ""); avail {
			fmt.Fprintf(conn, "AVAIL %s\n", latest)
		} else {
			fmt.Fprint(conn, "NONE\n")
		}
		return
	}
	if name == "OTA-APPLY" {
		handleOtaApply(conn, dest) // user tapped Install -> kick the su:s0 updater (download + update_engine + reboot)
		return
	}
	if name == "GET-OTA-AUTO" {
		fmt.Fprintf(conn, "%s\n", otaAutoGet()) // "1" if auto-install-while-charging is on, else "0"
		return
	}
	if name == "SET-OTA-AUTO" {
		// "SET-OTA-AUTO\n<0|1>\n" -> persist the auto-install-while-charging preference.
		v, _ := r.ReadString('\n')
		otaAutoSet(strings.TrimSpace(v) == "1")
		fmt.Fprint(conn, "OK\n")
		return
	}
	if name == "GET-OTA-TIME" {
		fmt.Fprintf(conn, "%s\n", otaTimeGet()) // "HH:MM" preferred install time, or "" = any time (next screen-off)
		return
	}
	if name == "SET-OTA-TIME" {
		// "SET-OTA-TIME\n<HH:MM|>\n" -> persist the OPTIONAL preferred install time; empty/invalid clears it
		// (= the default any-time / next-screen-off-while-charging behaviour).
		v, _ := r.ReadString('\n')
		otaTimeSet(strings.TrimSpace(v))
		fmt.Fprint(conn, "OK\n")
		return
	}
	if name == "GET-SYNC-INTERVAL" {
		fmt.Fprintf(conn, "%d\n", syncIntervalGet()) // effective continuous-seal cadence, in seconds
		return
	}
	if name == "SET-SYNC-INTERVAL" {
		// "SET-SYNC-INTERVAL\n<seconds|0>\n" -> persist the seal cadence; 0/invalid resets to the default.
		// syncLoop re-reads it each cycle, so the change applies live (no reboot).
		v, _ := r.ReadString('\n')
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		syncIntervalSet(n)
		fmt.Fprint(conn, "OK\n")
		return
	}
	if name == "GET-SYNC-STATUS" {
		// Backup health for the chooser's "not backed up" notification:
		//   IDLE  -- no live session (nothing to back up)
		//   OK    -- the last periodic seal pushed cleanly (or none needed yet since restore)
		//   STALE -- the last periodic seal FAILED (offline or store unreachable) -> changes at risk
		sn, _, su := readRoamSession(dest)
		switch {
		case sn == "" || su == "":
			fmt.Fprint(conn, "IDLE\n")
		case sealStatusGet() == "fail":
			fmt.Fprint(conn, "STALE\n")
		default:
			fmt.Fprint(conn, "OK\n")
		}
		return
	}
	if name == "RECOVER" {
		// "RECOVER\n<name>\n<mnemonic>\n<newpass>\n" -> reset the passphrase via the 12-word recovery code.
		rn, _ := r.ReadString('\n')
		rm, _ := r.ReadString('\n')
		rp, _ := r.ReadString('\n')
		if daemonVaultOp(func() error {
			return recoverVault("s3", strings.TrimRight(rn, "\r\n"), strings.TrimRight(rm, "\r\n"), strings.TrimRight(rp, "\r\n"))
		}) {
			fmt.Fprint(conn, "OK\n")
		} else {
			fmt.Fprint(conn, "BLANK\n")
		}
		return
	}
	if name == "ROTATE" {
		// "ROTATE\n<name>\n<oldpass>\n<newpass>\n" -> change the passphrase (no data re-encryption).
		rn, _ := r.ReadString('\n')
		ro, _ := r.ReadString('\n')
		rp, _ := r.ReadString('\n')
		rname, roldp, rnewp := strings.TrimRight(rn, "\r\n"), strings.TrimRight(ro, "\r\n"), strings.TrimRight(rp, "\r\n")
		if daemonVaultOp(func() error { return rotateVault("s3", rname, roldp, rnewp) }) {
			// If the LIVE roamed session is this identity, re-mark it with the new pass so the continuous
			// sync seals under the new credential -- the old ref was just invalidated, and a stale-pass sync
			// would re-create it as a legacy profile.
			if sn, sp, su := readRoamSession(dest); sn == rname && sp == roldp {
				markRoamSession(dest, rname, rnewp, su, "") // recovery path: no SE secret yet (enroll separately)
			}
			fmt.Fprint(conn, "OK\n")
		} else {
			fmt.Fprint(conn, "BLANK\n")
		}
		return
	}
	pass, _ := r.ReadString('\n')
	pass = strings.TrimRight(pass, "\r\n")
	items := 0
	valid := false
	if name != "" {
		os.RemoveAll(dest)
		os.MkdirAll(dest, 0o700)
		func() {
			defer func() { recover() }() // wrong pass -> ref/unseal fail -> recover -> valid stays false
			items, valid = restoreSet("s3", name, pass, dest, 1, false)
		}()
	}
	if valid {
		markSession(dest, name, pass) // a valid login -- even an empty (0-item) profile counts
		fmt.Fprintf(os.Stderr, "[logind] unlocked %q -> %d item(s)\n", name, items)
		fmt.Fprintf(conn, "OK %d\n", items)
	} else {
		os.RemoveAll(dest) // uniform clean blank on any failure (blind login)
		os.MkdirAll(dest, 0o700)
		fmt.Fprintf(os.Stderr, "[logind] blank (failed/empty) for %q\n", name)
		fmt.Fprint(conn, "BLANK\n")
	}
}

// otaAutoGet/otaAutoSet persist the "auto-install while charging" preference (DIA-20260618-06). Device-
// level: lives in /data/nowhere (alongside the store conf), so it survives reboots + the power-off
// user-data wipe and is cleared only by a factory reset; default off. Not user data, so it never roams.
const otaAutoPath = "/data/nowhere/ota-auto"

func otaAutoGet() string {
	if b, err := os.ReadFile(otaAutoPath); err == nil && strings.TrimSpace(string(b)) == "1" {
		return "1"
	}
	return "0"
}

func otaAutoSet(on bool) {
	if on {
		os.WriteFile(otaAutoPath, []byte("1\n"), 0o600)
	} else {
		os.Remove(otaAutoPath)
	}
}

// otaTimeGet/otaTimeSet persist an OPTIONAL preferred install time "HH:MM" for the auto-install path
// (DIA-20260619-01). Empty = no preference: install at the next screen-off while charging (the DIA-06
// default). Set = install around that local time while charging. Same /data/nowhere device-level storage
// as the auto flag (survives reboot + the power-off wipe, factory-reset-cleared, never roams).
const otaTimePath = "/data/nowhere/ota-time"

func otaTimeGet() string {
	b, err := os.ReadFile(otaTimePath)
	if err != nil {
		return ""
	}
	return validOtaTime(strings.TrimSpace(string(b)))
}

func otaTimeSet(v string) {
	v = validOtaTime(strings.TrimSpace(v))
	if v == "" {
		os.Remove(otaTimePath)
		return
	}
	os.WriteFile(otaTimePath, []byte(v+"\n"), 0o600)
}

// syncIntervalGet/Set persist the continuous-seal cadence in SECONDS (DIA-20260619-11), so the user can
// tune it from gate -> Settings. Effective value = the /data/nowhere/sync-interval file if set, else
// NOWHERE_SYNC_INTERVAL (conf/env), else the 120s default. Same device-level /data/nowhere storage as the
// OTA prefs (survives reboot + the power-off wipe, factory-reset-cleared, never roams). Lower = less data
// lost on an unclean power-off; convergent dedup keeps frequent cycles cheap. syncLoop re-reads each cycle.
const syncIntervalPath = "/data/nowhere/sync-interval"
const syncIntervalDefault = 120

func syncIntervalGet() int {
	if b, err := os.ReadFile(syncIntervalPath); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
			return clampSyncInterval(n)
		}
	}
	if s := os.Getenv("NOWHERE_SYNC_INTERVAL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return clampSyncInterval(n)
		}
	}
	return syncIntervalDefault
}

func syncIntervalSet(n int) {
	if n <= 0 {
		os.Remove(syncIntervalPath) // reset to the conf/env value, else the default
		return
	}
	os.WriteFile(syncIntervalPath, []byte(strconv.Itoa(clampSyncInterval(n))+"\n"), 0o600)
}

// clampSyncInterval keeps the cadence sane: a 15s floor so a stray value can't hammer the store/battery,
// and a 1-day ceiling.
func clampSyncInterval(n int) int {
	if n < 15 {
		return 15
	}
	if n > 86400 {
		return 86400
	}
	return n
}

// validOtaTime normalises a 24h "HH:MM" (00:00..23:59) to "%02d:%02d"; anything malformed -> "" (cleared).
func validOtaTime(s string) string {
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); n != 2 || err != nil {
		return ""
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return ""
	}
	return fmt.Sprintf("%02d:%02d", h, m)
}

// handleOtaApply triggers the su:s0 updater on a user-confirmed install: write the request into the RAM
// tmpfs + `ctl.start nowhere_otad` (the confined daemon's only way to spawn an su:s0 service; mirrors the
// roamd trigger). The updater re-checks, downloads (delta via the chunk cache), update_engine-applies, and
// reboots into the new slot -- so there is no result to wait for; the daemon just acks.
func handleOtaApply(conn net.Conn, dest string) {
	os.WriteFile(filepath.Join(dest, "ota.req"), []byte("apply\n"), 0o600)
	exec.Command("/system/bin/setprop", "ctl.start", "nowhere_otad").Run()
	fmt.Fprint(conn, "OK\n")
}

// handleCreate enrolls a NEW identity: seal an empty manifest under (name, pass) and log straight in.
// The ref is bound to (name, pass), so a collision needs the FULL credential -> refusing an existing
// one doesn't reveal which names exist (blind enrollment, same property as blind login).
func handleCreate(conn net.Conn, dest, name, pass string) {
	if name == "" || pass == "" {
		fmt.Fprint(conn, "BLANK\n")
		return
	}
	if !storeConfigured() {
		fmt.Fprint(conn, "NOSTORE\n") // no store set on this device yet -> the gate sends them to Settings/Store
		return
	}
	if ok, retry := enrollAllow(); !ok { // throttle open enrollment before touching the store
		fmt.Fprintf(conn, "RATELIMIT %d\n", retry)
		fmt.Fprintf(os.Stderr, "[logind] enroll throttled (retry %ds)\n", retry)
		return
	}
	if getRef("s3", headKey("s3", name, pass)) != "" {
		fmt.Fprint(conn, "EXISTS\n") // this exact (name, pass) is already enrolled
		return
	}
	mnemonic := ""
	ok := false
	noCredit := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if e, _ := r.(error); errors.Is(e, errInsufficientCredit) {
					noCredit = true // managed store + empty wallet -> say so, don't fake a "blank" error
				}
			}
		}()
		mnemonic = createVault("s3", name, pass) // new profile in the keyslot vault model -> a recovery code
		ok = true
	}()
	if ok {
		os.RemoveAll(dest)
		os.MkdirAll(dest, 0o700)
		markSession(dest, name, pass) // logged straight in to the (empty) new profile
		autoPublish(name, pass)       // so this new identity re-materializes on any device from name+pass
		fmt.Fprintf(os.Stderr, "[logind] created %q (vault)\n", name)
		fmt.Fprintf(conn, "OK 0 RECOVERY %s\n", mnemonic) // the chooser shows the 12 words once (slice 3)
	} else if noCredit {
		fmt.Fprint(conn, "NOCREDIT\n") // managed profile needs storage credit before it can be created
	} else {
		fmt.Fprint(conn, "BLANK\n")
	}
}

// handleEnrollSE hardens (name,pass) to this device's secure element (Endospore E.3b): the chooser passes
// the StrongBox-derived se_secret (hex); enrollSE adds the device `se` keyslot + drops the pass-only slot.
func handleEnrollSE(conn net.Conn, name, pass, seHex string) {
	if name == "" || pass == "" {
		fmt.Fprint(conn, "BLANK\n")
		return
	}
	if !storeConfigured() {
		fmt.Fprint(conn, "NOSTORE\n")
		return
	}
	sb, err := hex.DecodeString(seHex)
	if err != nil || len(sb) == 0 {
		fmt.Fprint(conn, "ERR-BADSECRET\n")
		return
	}
	if daemonVaultOp(func() error { return enrollSE("s3", name, pass, sb) }) {
		fmt.Fprintf(os.Stderr, "[logind] hardened %q to the secure element\n", name)
		fmt.Fprint(conn, "OK\n")
	} else {
		fmt.Fprint(conn, "ERR\n") // wrong pass / not a vault / store error
	}
}

// markSession records the session (name + passphrase) in a NON-dir marker in the root-only tmpfs
// state. STATUS reads it (so even an empty profile counts as ACTIVE), and the continuous sync + logoff
// read the passphrase from it to re-seal under the LIVE identity (not a baked one). It's a file, not a
// subdir, so the sync's push-set skips it -- the creds never roam -- and the tmpfs wipe (reboot/logoff)
// clears them. Trade-off: while logged in, root can read the passphrase here, but the decrypted data is
// right there too (no extra exposure), and both vanish the moment the session ends.
func markSession(dest, name, pass string) {
	os.WriteFile(filepath.Join(dest, ".session"), []byte(name+"\n"+pass+"\n"), 0o600)
}

// readSession parses (name, pass) from the .session marker; ("","") if there's no live session.
func readSession(dest string) (string, string) {
	b, err := os.ReadFile(filepath.Join(dest, ".session"))
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimRight(string(b), "\r\n"), "\n", 2)
	if len(parts) < 2 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// handleLogoff force-syncs the live session to the store (so nothing is lost), then wipes the tmpfs
// state -- the data LEAVES the device, stronger than a lock screen. Creds come from the .session marker
// (the only place the running session's passphrase lives). A failed final sync still wipes: the
// continuous sync already holds a recent copy, so logoff can never strand the user.
func handleLogoff(conn net.Conn, dest string) {
	name, pass := readSession(dest)
	if name != "" {
		func() {
			defer func() { recover() }()
			pushSet("s3", name, pass, dest)
		}()
	}
	os.RemoveAll(dest)
	os.MkdirAll(dest, 0o700)
	fmt.Fprintf(os.Stderr, "[logind] logged off %q\n", name)
	fmt.Fprint(conn, "OK\n")
}

// triggerRoamWorker hands the su:s0 data worker (nowhere_roamd) a request via the RAM tmpfs and kicks it
// with ctl.start -- the only way this confined daemon can spawn an su:s0 service -- then waits for the
// result. The worker does the privileged /data/user/N restore/seal + restorecon this domain can't.
// Returns the worker's result line ("OK <uid>" | "BLANK" | "ERR-...") or "ERR-TIMEOUT".
// triggerRoamWorker runs a roam worker op with the default (automatic) seal kind. See triggerRoamWorkerKind.
func triggerRoamWorker(dest, op, name, pass, uid, seSecret string, onProg func(string)) string {
	return triggerRoamWorkerKind(dest, op, name, pass, uid, seSecret, "", onProg)
}

// triggerRoamWorkerKind is triggerRoamWorker plus a #58 seal KIND ("manual" for a "Back up now", "" otherwise),
// passed to the worker on roam.req line 6 -> the worker exports it as NOWHERE_SEAL_KIND so the seal tags its
// snapshot manual (pinned) vs automatic (time-spaced).
func triggerRoamWorkerKind(dest, op, name, pass, uid, seSecret, kind string, onProg func(string)) string {
	workerMu.Lock() // one worker run at a time (login / logout / periodic seal share roam.req/res)
	defer workerMu.Unlock()
	// Clobber guard (DIA-20260625-07): once a reap is QUEUED for this uid, never run a seal ("out") for it --
	// the user is about to be / is being wiped, and sealing a half-wiped tree clobbers the good store ref (this
	// dropped OM's World map). Checked UNDER workerMu, the same lock seals serialize on, so it closes the race
	// where a periodic-sync "out" was already in-flight at logout and reaches here after the reap is queued. The
	// logout's OWN seal runs BEFORE it queues the reap (pendingReapUID still ""), so it is never skipped.
	if op == "out" && uid != "" {
		pendingReapMu.Lock()
		reaping := uid == pendingReapUID || uid == pendingSwitchUID
		pendingReapMu.Unlock()
		if reaping {
			return "SKIP-REAPING"
		}
	}
	req := filepath.Join(dest, "roam.req")
	res := filepath.Join(dest, "roam.res")
	prog := filepath.Join(dest, "roam.progress")
	os.Remove(res)
	os.Remove(prog) // clear any stale progress from a previous roam
	// roam.req line 5 = se_secret (hex) for a hardened identity; roamd exports it as NOWHERE_SE_SECRET so the
	// agent opens/seals via the `se` keyslot. Empty for non-hardened (agent falls back to the pass slot).
	if err := os.WriteFile(req, []byte(op+"\n"+name+"\n"+pass+"\n"+uid+"\n"+seSecret+"\n"+kind+"\n"), 0o600); err != nil {
		return "ERR-REQ"
	}
	exec.Command("/system/bin/setprop", "ctl.start", "nowhere_roamd").Run()
	last := ""
	// Adaptive wait: a large first seal / restore (e.g. a 175 MB map set over Wi-Fi) runs for minutes, so don't
	// cap on TOTAL time -- give up only after a STALL (no new progress for stallTicks) or a generous hard cap.
	// Progress is read every tick regardless of onProg, both to drive the caller's bar and to detect the stall.
	// (DIA-20260625-04: the old fixed ~60 s cap returned ERR-TIMEOUT mid-upload on a big logoff seal.)
	const stallTicks = 300 // 150 s with no new progress -> assume hung
	const hardCap = 2400   // 20 min absolute ceiling (×500 ms)
	idle := 0
	beat := 0
	for i := 0; i < hardCap; i++ {
		time.Sleep(500 * time.Millisecond)
		if b, err := os.ReadFile(prog); err == nil {
			if s := strings.TrimRight(string(b), "\r\n"); s != "" && s != last {
				last = s
				idle = 0
				if onProg != nil { // relay the worker's chunk progress to the caller (the restore / logoff bar)
					onProg(s)
				}
			}
		}
		// HEARTBEAT (DIA-20260630-43, #75): during a long no-NEW-progress stretch (the CPU-heavy unpack of a big
		// restore), re-send the last line every ~10s so the caller's socket read + the login's rolling deadline
		// don't starve and time the login out -- a 2.9 GB restore's quiet phase outlasted the gate's 120s read
		// timeout. Does NOT reset idle, so the genuine-stall watchdog below stays honest.
		if beat++; onProg != nil && last != "" && beat%20 == 0 {
			onProg(last)
		}
		if b, err := os.ReadFile(res); err == nil {
			if out := strings.TrimRight(string(b), "\r\n"); out != "" {
				os.Remove(res)
				os.Remove(prog)
				return out
			}
		}
		idle++
		if idle >= stallTicks {
			return "ERR-TIMEOUT"
		}
	}
	return "ERR-TIMEOUT"
}

// handleRoamIn drives the worker to restore profile (name,pass) into the chooser-created user <uid>, and
// -- ONLY on success -- records the roam session so logout re-seals the SAME user. A bad cred / missing
// profile yields an empty user (BLANK, indistinguishable -- blind login) and is NOT recorded, so logout
// can never seal an empty user over a real profile.
func handleRoamIn(conn net.Conn, dest, name, pass, uid, seSecret string) {
	if name == "" || uid == "" {
		fmt.Fprint(conn, "BLANK\n")
		return
	}
	if !storeConfigured() {
		tryDiscover(name, pass) // fresh device: bootstrap the data store from name+passphrase via discovery
	}
	if !storeConfigured() {
		fmt.Fprint(conn, "NOSTORE\n") // no store + nothing discovered -> the gate points to Settings
		return
	}
	// ROLLING deadline (DIA-20260630-43, #75): a large restore streams progress for MINUTES -- a 2.9 GB china
	// map restore took ~6 min, but the OLD fixed 150 s TOTAL deadline fired at ~2.5 min and killed the login
	// conn, so the gate showed "couldn't sign in" even though the restore SUCCEEDED (full map landed on disk).
	// Reset the deadline on every progress line so the connection lives as long as progress FLOWS (a 150 s
	// STALL cap, matching triggerRoamWorker's stall watchdog), then a fresh window for the post-restore reply.
	conn.SetDeadline(time.Now().Add(150 * time.Second))
	// Stream the worker's restore progress to the gate as "PROGRESS <phase> <done> <total>" lines; the
	// chooser drives a determinate bar off them, then reads the final OK/BLANK line.
	out := triggerRoamWorker(dest, "in", name, pass, uid, seSecret, func(p string) {
		conn.SetDeadline(time.Now().Add(150 * time.Second)) // roll forward while progress flows
		fmt.Fprintf(conn, "PROGRESS %s\n", p)
	})
	conn.SetDeadline(time.Now().Add(120 * time.Second)) // fresh window for the post-restore reply (head fetch, migrate, wallet)
	if strings.HasPrefix(out, "OK") {
		// THROWAWAY detection (#4, DIA-20260626-04): no head in the store for (name,pass) means this login
		// restored nothing -> it's a LOCAL-ONLY session (never sealed, destroyed on logoff) until PROMOTE saves
		// it. A real profile has a head. Reuses the head fetch that the LastSeal welcome-back needs anyway.
		head := getRef("s3", headKey("s3", name, pass))
		markRoamSession(dest, name, pass, uid, seSecret)
		if head == "" {
			markLocalSession(dest) // gates every seal OFF; the gate's Profile screen offers "Save to your store"
			fmt.Fprintf(os.Stderr, "[logind] roam-in %q -> user %s (LOCAL/throwaway -- not sealed)\n", name, uid)
			fmt.Fprintf(conn, "%s\n", out) // log in exactly like a real profile (blind: indistinguishable)
			return
		}
		clearLocalSession(dest) // defensive: a resolved login is never local
		// Auto-upgrade a legacy profile to the keyslot vault on first login (no re-encryption).
		recovery := ""
		func() {
			defer func() { recover() }()
			if m, migrated := migrateVault("s3", name, pass); migrated {
				recovery = m
			}
		}()
		autoPublish(name, pass) // keep discovery in sync: this identity -> the current store config
		func() { // restore the roaming token wallet (B.3) so this session can keep its data leased; best-effort
			defer func() { recover() }()
			if dk := resolveDK("s3", name, pass); dk != nil {
				restoreWallet("s3", name, pass, dk, walletPathFor(dest))
			}
		}()
		fmt.Fprintf(os.Stderr, "[logind] roam-in %q -> user %s (migrated=%v)\n", name, uid, recovery != "")
		// "Last active" for the welcome-back: the vault head carries LastSeal (unix time of the previous
		// session's last push). Legacy/just-migrated heads (LastSeal=0) simply omit it. (DIA-20260625-05)
		func() {
			defer func() { recover() }()
			if vv, isV := parseVault(getBlob("s3", head)); isV && vv.LastSeal > 0 {
				fmt.Fprintf(conn, "LASTSEAL %d\n", vv.LastSeal)
			}
		}()
		if recovery != "" {
			fmt.Fprintf(conn, "%s RECOVERY %s\n", out, recovery) // "OK <uid> RECOVERY <12 words>" -> shown once
		} else {
			fmt.Fprintf(conn, "%s\n", out)
		}
	} else {
		fmt.Fprintf(os.Stderr, "[logind] roam-in BLANK %q (%s)\n", name, out)
		fmt.Fprint(conn, "BLANK\n")
	}
}

// handleRoamOut seals the live roamed user's /data/user/N back to the store (creds+uid from the roam
// session marker), then clears the marker. The chooser stops+removes the user after we reply.
func handleRoamOut(conn net.Conn, dest string) {
	name, pass, uid := readRoamSession(dest)
	if name == "" || uid == "" {
		fmt.Fprint(conn, "OK\n") // no live roam session -> nothing to seal
		return
	}
	if isLocalSession(dest) {
		// #4 (DIA-20260626-04): a throwaway is never sealed -> reboot wipes the local user, nothing to push.
		clearLocalSession(dest)
		fmt.Fprintf(os.Stderr, "[logind] roam-out %q user %s: LOCAL/throwaway -> reboot WITHOUT seal\n", name, uid)
	} else {
		out := triggerRoamWorker(dest, "out", name, pass, uid, readRoamSessionSE(dest), nil) // logoff: no progress UI (we reboot after)
		fmt.Fprintf(os.Stderr, "[logind] roam-out %q user %s (%s)\n", name, uid, out)
		sealWalletBestEffort(dest, name, pass) // roam any token-wallet changes before the wipe (B.3)
	}
	os.Remove(filepath.Join(dest, ".roamsession"))
	fmt.Fprint(conn, "OK\n")
	// Data is sealed -> reboot to WIPE the ephemeral roamed user and bring the gate back up. The chooser
	// can't reboot from a secondary user (PowerManager.reboot is blocked there); this root daemon can, via
	// sys.powerctl. Brief pause so the OK reply reaches the chooser before init tears us down.
	time.Sleep(500 * time.Millisecond)
	exec.Command("/system/bin/setprop", "sys.powerctl", "reboot").Run()
}

// handlePromote (#4, DIA-20260626-04): "Save to your store" -- turn the live LOCAL/throwaway session into a
// real roaming profile WITHOUT interrupting it. Create the store vault for the session's own (name,pass) (the
// recovery code), drop the .localsession marker so the sync + logoff persist it from here, then seal the live
// data in the background (like logout, so the daemon's accept loop stays free; workerMu serializes it with the
// periodic sync). The session keeps running -- only its "saved?" status changes.
func handlePromote(conn net.Conn, dest string) {
	name, pass, uid := readRoamSession(dest)
	if name == "" || uid == "" {
		fmt.Fprint(conn, "ERR-NOSESSION\n")
		return
	}
	if !isLocalSession(dest) {
		fmt.Fprint(conn, "ERR-ALREADY\n") // already a stored, roaming profile -> nothing to promote
		return
	}
	if !storeConfigured() {
		fmt.Fprint(conn, "NOSTORE\n") // can't save without a store configured
		return
	}
	if getRef("s3", headKey("s3", name, pass)) != "" {
		fmt.Fprint(conn, "ERR-EXISTS\n") // defensive: never clobber an existing profile (a throwaway has no head)
		return
	}
	recovery := ""
	noCredit := false
	if !func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				if e, _ := r.(error); errors.Is(e, errInsufficientCredit) {
					noCredit = true // throwaway has no credit yet -> the chooser offers Add-credits, then retries
				} else {
					fmt.Fprintf(os.Stderr, "[logind] promote vault %q: %v\n", name, r)
				}
				ok = false
			}
		}()
		recovery = createVault("s3", name, pass) // store vault + 12-word recovery code
		return true
	}() {
		if noCredit {
			fmt.Fprint(conn, "NOCREDIT\n") // needs storage credit to save -> Add-credits then Save again
		} else {
			fmt.Fprint(conn, "ERR-VAULT\n")
		}
		return
	}
	clearLocalSession(dest) // now a real roaming profile: the sync + logoff persist it from here on
	autoPublish(name, pass) // publish discovery: this identity -> the current store config
	resetSealStatus()
	fmt.Fprintf(os.Stderr, "[logind] promote %q user %s -> stored + roaming (sealing in background)\n", name, uid)
	fmt.Fprintf(conn, "OK RECOVERY %s\n", recovery)
	go func() {
		defer func() { recover() }()
		syscall.Sync() // flush lazy app writes before the first seal (same reason as logout)
		out := triggerRoamWorker(dest, "out", name, pass, uid, readRoamSessionSE(dest), nil)
		recordSeal(strings.HasPrefix(out, "OK"))
		fmt.Fprintf(os.Stderr, "[logind] promote seal %q user %s -> %s\n", name, uid, out)
	}()
}

// handleLogout is the reboot-free logoff: seal the live roamed user (same final push as ROAM-OUT), clear
// the session, then hand the in-place teardown to the USER-0 CHOOSER (the device owner). The su:s0 worker
// CANNOT do the AM user-lifecycle ops -- switch-user/stop-user hit the device-owner wall (Failed
// transaction) -- and this confined daemon can't run am at all; only the chooser, as device owner, can.
// We can't push to the chooser (the accept loop is serial, so a parked connection would deadlock it), so
// the chooser POLLS: we record a pending-reap uid that its next POLL-REAP picks up, and it stops+removes
// the ephemeral user (deleting /data/user|user_de|media/N; FBE key destruction == the reboot-wipe). If no
// chooser is polling (its process died), an 8s fallback reboots -- still wipes the ephemeral user. ROAM-OUT
// (reboot) stays as the "secure logoff" that also clears all in-RAM state.
var pendingReapMu sync.Mutex
var pendingSwitchUID string // phase 1: chooser switches to the gate (fast, before the seal)
var pendingReapUID string   // phase 2a: chooser removes the user (after the seal) -- LOGOFF (crypto-shred)
var pendingLockUID string   // phase 2b: chooser STOPS the user (cold-lock, P3) -- keep /data, FBE-locked, resumable
var sealInFlight bool       // true while the background logoff seal runs -> POLL-REAP reports "SEALING <prog>"
var sealProg string         // latest "<phase> <done> <total>" from the seal, for the gate's "Saving…" bar (DIA-20260625-04)
var sealFail bool           // #86: the last logoff seal FAILED -> SEAL-STATUS reports FAILED so the session un-sticks and we do NOT reap unsealed data

// nextReapAction hands the user-0 chooser its next no-reboot-logoff step (once each), newest phase first:
// "SWITCH <uid>" to switch to the gate FAST (before the S3 seal -> no window to gesture out of the
// "Logging off…" screen), then "REAP <uid>" to remove the user (only AFTER the seal -> seal-before-wipe is
// preserved), else "NONE". Split into two phases so the gate reclaims the foreground without waiting on S3.
func nextReapAction() string {
	pendingReapMu.Lock()
	defer pendingReapMu.Unlock()
	if pendingSwitchUID != "" {
		u := pendingSwitchUID
		pendingSwitchUID = ""
		return "SWITCH " + u
	}
	if pendingReapUID != "" {
		u := pendingReapUID
		pendingReapUID = ""
		return "REAP " + u
	}
	if pendingLockUID != "" { // P3 cold-lock: stop (not remove) -> FBE-lock /data, resumable
		u := pendingLockUID
		pendingLockUID = ""
		return "LOCK " + u
	}
	return "NONE"
}

func handleLogout(conn net.Conn, dest string) {
	name, pass, uid := readRoamSession(dest)
	if name == "" || uid == "" {
		fmt.Fprint(conn, "OK\n") // no live roam session -> nothing to do
		return
	}
	if isLocalSession(dest) {
		// #4 (DIA-20260626-04): a throwaway is DESTROY-ON-LOGOFF with NO seal -- it was never written to the
		// store. Clear the markers and queue the switch+reap straight away (no "Saving…" -- nothing to save). The
		// user-0 watcher's POLL-REAP then switches to the gate + removeUser, destroying /data/user/N.
		os.Remove(filepath.Join(dest, ".roamsession"))
		clearLocalSession(dest)
		pendingReapMu.Lock()
		pendingSwitchUID = uid
		pendingReapUID = uid
		pendingReapMu.Unlock()
		fmt.Fprintf(os.Stderr, "[logind] logout %q user %s: LOCAL/throwaway -> reap WITHOUT seal\n", name, uid)
		fmt.Fprint(conn, "OK\n")
		return
	}
	// The SE secret opens a hardened (Endospore) vault's `se` slot for the seal; read it BEFORE clearing
	// .roamsession (readRoamSessionSE on a cleared marker returns "" -> "no slot opens" -> a silent strand).
	seSecret := readRoamSessionSE(dest)
	// Clear .roamsession FIRST so the periodic sync can't race the final seal -- and, critically, can't fire
	// AGAIN after the reap is queued and seal a HALF-WIPED user. Keeping it alive during logoff (the -04b
	// auto-save experiment) did exactly that: a post-logout periodic seal of the reaping user 11 clobbered a
	// 318-chunk ref down to 305 and dropped OM's World map. The cost of reverting: dismissing the "Saving…"
	// screen + continuing to work loses that post-dismiss work -- which the screen explicitly warns against.
	// (DIA-20260625-07; reverts -04b.)
	os.Remove(filepath.Join(dest, ".roamsession"))
	// Reply AT ONCE + seal in the BACKGROUND, keeping the user in their SESSION watching "Saving… N%" (the
	// LogoffActivity polls SEAL-STATUS) -- NOT dumped to the gate during the upload. Sealing in a goroutine
	// (not inline) is what avoids the hang: the single-threaded daemon stays free to serve those SEAL-STATUS
	// polls AND the watcher's POLL-REAP. Only AFTER the seal lands do we queue the switch + reap
	// (seal-before-wipe), which the watcher then fires unstarved. (DIA-20260625-04, session-side -- the
	// synchronous version starved the reap; the gate-side version dumped the user to the gate mid-upload.)
	pendingReapMu.Lock()
	sealInFlight = true
	sealFail = false // #86: fresh logoff -> clear any prior FAILED status
	sealProg = ""
	pendingReapMu.Unlock()
	fmt.Fprintf(os.Stderr, "[logind] logout %q user %s: sealing in background (session shows progress)\n", name, uid)
	fmt.Fprint(conn, "OK\n")
	// #86 Fix C: overall logoff watchdog. If the seal is STILL in flight long past triggerRoamWorker's own
	// 20-min hard cap, the seal goroutine is genuinely hung -> reboot (a boot wipes the ephemeral user = a
	// completed logoff; data is safe at the last good head). The 22-min bound is safely beyond the worker cap,
	// so it never interrupts a legit seal (which clears sealInFlight before then). This is the catch-all for a
	// stall that isn't a clean seal FAILURE (which Fix A un-sticks) or a stuck reap (which Fix B reboots).
	go func() {
		time.Sleep(22 * time.Minute)
		pendingReapMu.Lock()
		hung := sealInFlight
		pendingReapMu.Unlock()
		if hung {
			fmt.Fprintf(os.Stderr, "[logind] logout seal still in flight after 22m (hung) -> reboot fallback\n")
			exec.Command("/system/bin/setprop", "sys.powerctl", "reboot").Run()
		}
	}()
	go func() {
		// Apps persist SharedPreferences / SQLite LAZILY on onStop, so reading /data/user/N the instant LOGOUT
		// arrives can miss the LAST change. Give that flush a moment + syscall.Sync() for durability, THEN seal
		// (DIA-20260623-01); the worker streams chunk progress into sealProg for the session's "Saving…" bar.
		time.Sleep(2 * time.Second)
		syscall.Sync()
		out := triggerRoamWorker(dest, "out", name, pass, uid, seSecret, func(p string) {
			pendingReapMu.Lock()
			sealProg = p
			pendingReapMu.Unlock()
		})
		if out != "OK" {
			// #86 Fix A (data-safety): the logoff seal FAILED (ERR-TIMEOUT/ERR-PUSH). Do NOT queue the reap --
			// reaping would remove the user and WIPE its unsealed changes (and could race a still-running agent
			// push). Keep the session SIGNED IN: restore the .roamsession marker (so the periodic sync resumes and
			// a retry logoff can seal again) and report FAILED so the logging-off screen un-sticks back to the
			// session instead of hanging on "Saving…" forever.
			pendingReapMu.Lock()
			sealInFlight = false
			sealFail = true
			pendingReapMu.Unlock()
			markRoamSession(dest, name, pass, uid, seSecret)
			fmt.Fprintf(os.Stderr, "[logind] logout seal %q user %s FAILED (%s) -> staying signed in, NOT reaping\n", name, uid, out)
			return
		}
		pendingReapMu.Lock()
		sealInFlight = false
		pendingSwitchUID = uid // seal landed -> NOW switch to the gate + remove (seal-before-wipe preserved)
		pendingReapUID = uid
		pendingReapMu.Unlock()
		fmt.Fprintf(os.Stderr, "[logind] logout seal %q user %s (OK); switch+reap queued\n", name, uid)
		// #86 Fix B: reboot fallback covering BOTH reap phases. Phase 1: the SWITCH not consumed in 8s (no live
		// watcher) -> reboot to wipe+gate.
		time.Sleep(8 * time.Second)
		pendingReapMu.Lock()
		switchStuck := pendingSwitchUID == uid
		if switchStuck {
			pendingSwitchUID = ""
			pendingReapUID = ""
		}
		pendingReapMu.Unlock()
		if switchStuck {
			fmt.Fprintf(os.Stderr, "[logind] switch not consumed in 8s (no watcher) -> reboot fallback\n")
			exec.Command("/system/bin/setprop", "sys.powerctl", "reboot").Run()
			return
		}
		// Phase 2: the switch happened but the REMOVE didn't (watcher switched then stalled) -> reboot, so the
		// logoff never strands as an orphaned running user + a "Signing out…" gate. (Old code only covered phase 1.)
		time.Sleep(12 * time.Second)
		pendingReapMu.Lock()
		reapStuck := pendingReapUID == uid
		if reapStuck {
			pendingReapUID = ""
		}
		pendingReapMu.Unlock()
		if reapStuck {
			fmt.Fprintf(os.Stderr, "[logind] remove not consumed 12s after switch -> reboot fallback\n")
			exec.Command("/system/bin/setprop", "sys.powerctl", "reboot").Run()
		}
	}()
}

// handleColdLock cold-locks the live roamed session (P3, DIA-20260625-13; docs/resumable-session.md): record a
// .coldlock marker so the gate can offer RESUME, clear .roamsession (a locked CE can't be sealed -> stop the
// periodic sync), and queue the switch-to-gate + STOP (not remove) for the user-0 chooser. NO seal: the periodic
// sync already backed the data up, so the lock is fast; the chooser's stopUser evicts the CE key -> /data is
// FBE-encrypted at rest, resumable. (The cost of skipping the seal: post-last-sync changes are not in the store,
// but a power-off boot-wipes the local copy anyway -- the amnesiac contract.)
func handleColdLock(conn net.Conn, dest string) {
	name, pass, uid := readRoamSession(dest)
	if name == "" || uid == "" {
		fmt.Fprint(conn, "OK\n") // no live session -> nothing to lock
		return
	}
	se := readRoamSessionSE(dest)
	// Self-arm the lockscreen credential (= the session passphrase the daemon already holds in .roamsession)
	// BEFORE stopping the user, so a MANUAL lock right after login is still passphrase-protected + resumable
	// with zero user action (no need to have screen-off'd first). Runs while user N is still the live session
	// (set-password needs it running). Best-effort + idempotent: the idle path already armed it on screen-off,
	// so this set-password harmlessly no-ops there. (DIA-20260625-13)
	triggerRoamWorker(dest, "armcred", name, pass, uid, se, nil)
	// The marker carries name+uid(+se) so the gate shows "Resume <name>" and resume can re-arm the session. NOT
	// the pass: resume re-derives it from the freshly typed passphrase, so no credential sits at rest.
	os.WriteFile(filepath.Join(dest, ".coldlock"), []byte(name+"\n"+uid+"\n"+se+"\n"), 0o600)
	os.Remove(filepath.Join(dest, ".roamsession")) // stop the sync (a cold-locked CE is unreadable)
	pendingReapMu.Lock()
	pendingSwitchUID = uid // phase 1: switch to the gate
	pendingLockUID = uid   // phase 2b: STOP (not remove) -> FBE-lock the user
	pendingReapMu.Unlock()
	fmt.Fprintf(os.Stderr, "[logind] cold-lock %q user %s: switch+lock queued (resumable)\n", name, uid)
	fmt.Fprint(conn, "OK\n")
}

// readColdLock parses the .coldlock marker handleColdLock wrote (name\nuid\nse\n, NO pass). Returns empty
// strings when there is no resumable session (no marker / malformed), which the callers treat as "NONE".
func readColdLock(dest string) (name, uid, se string) {
	b, err := os.ReadFile(filepath.Join(dest, ".coldlock"))
	if err != nil {
		return "", "", ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n")
	if len(lines) < 2 || lines[0] == "" || lines[1] == "" {
		return "", "", ""
	}
	if len(lines) >= 3 {
		se = lines[2]
	}
	return lines[0], lines[1], se
}

// handleGetColdLock tells the gate whether a cold-locked session is waiting, so it can render the
// "Welcome back <name>" resume prompt instead of (or above) the fresh-login form. uid is sent FIRST so a
// profile name containing spaces survives the chooser's single-split parse.
func handleGetColdLock(conn net.Conn, dest string) {
	name, uid, _ := readColdLock(dest)
	if name == "" {
		fmt.Fprint(conn, "NONE\n")
		return
	}
	fmt.Fprintf(conn, "COLDLOCK %s %s\n", uid, name)
}

// handleResume is the cold-lock counterpart of login. The gate sends the freshly typed passphrase; we
// verify it against the cold-locked user's CE via the su:s0 worker (which decrypts the FBE storage in
// place -- verify BEFORE the chooser switches into the user, the recipe that avoids the disabled-keyguard
// timeout crash). Only on success do we re-arm .roamsession (so the periodic sync + logout track this
// user again) and drop the .coldlock marker; the chooser then switches into the now-unlocked user. A
// wrong pass leaves the CE cold and replies WRONGPASS, exposing nothing beyond what the marker showed.
func handleResume(conn net.Conn, dest, pass string) {
	name, uid, se := readColdLock(dest)
	if name == "" {
		fmt.Fprint(conn, "NONE\n") // no cold-locked session (e.g. a stale resume tap) -> nothing to do
		return
	}
	if pass == "" {
		fmt.Fprint(conn, "WRONGPASS\n")
		return
	}
	out := triggerRoamWorker(dest, "verify", name, pass, uid, se, nil)
	if out != "OK" {
		fmt.Fprintf(os.Stderr, "[logind] resume %q user %s: verify %s\n", name, uid, out)
		fmt.Fprint(conn, "WRONGPASS\n")
		return
	}
	// CE decrypted: re-arm the live session (name+pass+uid+se) so sealing/logout re-find this SAME user,
	// drop the cold-lock marker, and clear any pending lock/reap so the watcher can't re-lock the user the
	// chooser is about to switch into (the bounce-and-reap that shredded the ad-hoc test).
	markRoamSession(dest, name, pass, uid, se)
	os.Remove(filepath.Join(dest, ".coldlock"))
	pendingReapMu.Lock()
	if pendingLockUID == uid {
		pendingLockUID = ""
	}
	if pendingReapUID == uid {
		pendingReapUID = ""
	}
	if pendingSwitchUID == uid {
		pendingSwitchUID = ""
	}
	pendingReapMu.Unlock()
	fmt.Fprintf(os.Stderr, "[logind] resume %q user %s: CE unlocked, session re-armed\n", name, uid)
	fmt.Fprintf(conn, "OK %s\n", uid)
}

// deleteForSession executes a "Delete this profile" request against the store and classifies the result so
// the caller can pick the right UX. (name,pass) is the TYPED confirm credential; (loginName,loginPass) are
// the SESSION's login creds (from .roamsession; "" when no stored profile backs the session). Returns:
//   "deleted"   -- the typed credential resolved a profile; its head + recovery refs were dropped.
//   "wrongpass" -- the session HAS a real stored profile, but the typed confirm credential didn't resolve it
//                  (a mistyped confirm). The caller must NOT treat this as done: a wrong pass must never look
//                  like a successful delete, or a typo would "delete" a profile that actually survives (the
//                  DIA-20260616-50 guard). profileRef is keyed on (name,pass), so a wrong pass and a missing
//                  profile are BOTH head=="" -- we disambiguate via the LOGIN creds, which still resolve.
//   "noop"      -- no stored profile backs this session: a throwaway blind login, or a profile already gone
//                  (e.g. a deleted profile re-entered via a stale-read login). Nothing to delete; the caller
//                  carries on (reaps to the gate) exactly as if it had deleted -- the blind model hides which
//                  case it was, and re-deleting an already-gone profile shouldn't read as "wrong passphrase"
//                  (DIA-20260617-04).
func deleteForSession(base, name, pass, loginName, loginPass string) string {
	if deleteProfile(base, name, pass) {
		return "deleted"
	}
	if loginName != "" && getRef(base, headKey(base, loginName, loginPass)) != "" {
		return "wrongpass"
	}
	return "noop"
}

// handleDelete is the user-facing "delete my profile" (from the logged-in gate's logoff screen). The typed
// passphrase re-authenticates via deleteForSession: a REAL profile + a WRONG confirm pass -> NOTFOUND (nothing
// happens); a correct pass deletes; and a session with NO stored profile (throwaway / already-gone) is a no-op
// that still carries on. On anything but a wrong pass the LOCAL session is wiped WITHOUT a final seal (sealing
// would re-create a deleted head) via the two-phase reap queued at once -- the user-0 chooser switches to the
// gate, then removes the ephemeral user.
func handleDelete(conn net.Conn, dest, name, pass string) {
	if name == "" || pass == "" {
		fmt.Fprint(conn, "BLANK\n")
		return
	}
	ln, lp, uid := readRoamSession(dest)
	switch deleteForSession("s3", name, pass, ln, lp) {
	case "wrongpass":
		fmt.Fprint(conn, "NOTFOUND\n") // real profile, wrong confirm passphrase -> nothing deleted, no reap
		return
	case "deleted":
		fmt.Fprintf(os.Stderr, "[logind] deleted profile %q from the store\n", name)
	default: // "noop": no stored profile backs this session (throwaway / already-gone) -> carry on + reap
		fmt.Fprintf(os.Stderr, "[logind] delete: no stored profile backs this session -> reap + carry on\n")
	}
	os.Remove(filepath.Join(dest, ".roamsession")) // so the periodic sync can't reseal the now-deleted profile
	if uid != "" {
		pendingReapMu.Lock()
		pendingSwitchUID = uid // phase 1: switch to the gate
		pendingReapUID = uid   // phase 2: remove the user -- queued NOW (no seal to wait on)
		pendingReapMu.Unlock()
		go func() { // reboot fallback if no chooser is polling to do the reap
			time.Sleep(8 * time.Second)
			pendingReapMu.Lock()
			stuck := pendingSwitchUID == uid
			if stuck {
				pendingSwitchUID = ""
				pendingReapUID = ""
			}
			pendingReapMu.Unlock()
			if stuck {
				fmt.Fprintf(os.Stderr, "[logind] delete reap not consumed in 8s -> reboot fallback\n")
				exec.Command("/system/bin/setprop", "sys.powerctl", "reboot").Run()
			}
		}()
	}
	fmt.Fprint(conn, "OK\n")
}

// handleGetApps returns the roamed app list (one package per line) the worker surfaced to apps.out on the
// last roam-in, so the chooser can reinstall the user's apps. Empty when there is no list.
func handleGetApps(conn net.Conn, dest string) {
	if b, err := os.ReadFile(filepath.Join(dest, "apps.out")); err == nil {
		conn.Write(b)
	}
}

// handleGetPrefs hands back the roamed-in profile's timezone + locale (the worker surfaced them from the
// sealed CE data to prefs.out, mirroring apps.out). The user-0 chooser applies them on login -- tz via the
// device owner, locale via LocalePicker -- which the su:s0 worker cannot do (DIA-20260616-58).
func handleGetPrefs(conn net.Conn, dest string) {
	if b, err := os.ReadFile(filepath.Join(dest, "prefs.out")); err == nil {
		conn.Write(b)
	}
}

// handleSetTz applies a timezone LIVE via the su:s0 worker. Only root can set the global system zone -- a
// secondary-user app (the in-session picker) and this confined daemon cannot -- so the worker runs
// `cmd alarm set-timezone` (DIA-20260616-60). zone="" => Automatic (auto-detection on, baked default floor).
// The override also rides the roamed prefs (the chooser wrote it), so it follows the identity on future logins.
func handleSetTz(conn net.Conn, dest, zone string) {
	out := triggerRoamWorker(dest, "settz", zone, "", "", "", nil) // zone in the name slot; pass/uid/se unused
	fmt.Fprintf(conn, "%s\n", out)
}

// handleGrantPerms re-grants the roamed runtime permissions for user <uid> via the su:s0 worker (root pm
// grant -- the chooser doesn't hold GRANT_RUNTIME_PERMISSIONS, and a perm can only be granted to an installed
// app, so the chooser triggers this right after install-existing). uid rides the name slot too so the worker's
// non-empty-name arg check passes; the worker reads $USERDIR from the uid slot. (DIA-20260625-06)
func handleGrantPerms(conn net.Conn, dest, uid string) {
	out := triggerRoamWorker(dest, "grant", uid, "", uid, "", nil)
	fmt.Fprintf(conn, "%s\n", out)
}

// handleResolveIpTz replies "tz=<olson>\n" with a coarse timezone derived from the device's PUBLIC IP, or
// "tz=\n" on any failure. This is the tier-4 IP fallback (docs/timezone-model.md): the LAST resort the chooser
// uses as the AUTOMATIC seed when a profile has no override AND no NITZ/geo signal has resolved. The agent (not
// the chooser) does the network egress -- it's the one component already allowed out under sepolicy, and this
// keeps all external calls in one place (a future least-knowledge gateway can proxy it). It NEVER pins the zone
// or disables auto-detection: the chooser applies it with auto still on, so a real NITZ/geo fix overrides it.
// Privacy caveat (by design, accepted for tier 4): this leaks the public IP to the resolver and returns the VPN
// exit node's zone behind a VPN -- which is why it's best-effort only and the manual override stays the anchor.
func handleResolveIpTz(conn net.Conn) {
	fmt.Fprintf(conn, "tz=%s\n", resolveIpTimezone())
}

// resolveIpTimezone does a single short HTTPS GET to a no-auth public IP-geolocation service and extracts the
// caller's IANA/Olson zone. Returns "" on any error/timeout/implausible value; the caller treats "" as "no IP
// zone" and keeps the baked default. The chooser independently validates the zone against the platform's known
// IDs before applying, so a bogus value here is harmless. Uses ipinfo.io's JSON (top-level "timezone") because
// it's HTTPS, no-key, and -- unlike ipapi.co, which 429s the default Go user-agent -- it answers a plain client
// without us having to spoof a browser UA (which would be brittle + against ToS) or leak a "Nowhere" UA.
func resolveIpTimezone() string {
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Get("https://ipinfo.io/json")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8192)) // the JSON doc is small
	if err != nil {
		return ""
	}
	var out struct {
		Timezone string `json:"timezone"`
	}
	if json.Unmarshal(b, &out) != nil {
		return ""
	}
	z := strings.TrimSpace(out.Timezone)
	// Sanity-check the shape of an Olson zone ("Area/Location", ASCII, no spaces/markup) so a stray field or
	// error body can never be handed back as a "zone".
	if z == "" || len(z) > 48 || !strings.Contains(z, "/") {
		return ""
	}
	for _, c := range z {
		ok := c == '/' || c == '_' || c == '-' || c == '+' ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if !ok {
			return ""
		}
	}
	return z
}

// --- Store config (Tier 2): set/inspect WHERE the device saves its roaming data. The login daemon now
// starts even with no conf so the Settings/Store screen can configure one in-place (a fresh free-OS flash
// ships with no store). The conf lives at /data/nowhere/nowhere.conf -- device-level, persists the
// power-off wipe (only the per-user roamed data is amnesiac) and is rotatable without a reflash. ---

const storeConfPath = "/data/nowhere/nowhere.conf"

// storeConfigured reports whether the daemon has a usable store: a managed (cap) gateway, OR a full S3
// config (endpoint + bucket + creds). In managed/cap mode there are NO S3 creds -- the gateway IS the store
// (cap-routed I/O) -- so a GATEWAY_URL with no creds is a complete, login-able config.
func storeConfigured() bool {
	if capMode() {
		return true
	}
	return os.Getenv("S3_ENDPOINT") != "" && os.Getenv("S3_BUCKET") != "" &&
		os.Getenv("S3_ACCESS_KEY") != "" && os.Getenv("S3_SECRET_KEY") != ""
}

// handleGetStore reports the current store config to the Settings screen -- endpoint/region/bucket and
// whether a key is set, but NEVER the secret key.
func handleGetStore(conn net.Conn) {
	keyset := "no"
	if os.Getenv("S3_ACCESS_KEY") != "" && os.Getenv("S3_SECRET_KEY") != "" {
		keyset = "yes"
	}
	configured := "no"
	if storeConfigured() {
		configured = "yes"
	}
	fmt.Fprintf(conn, "configured=%s\nendpoint=%s\nregion=%s\nbucket=%s\nkeyset=%s\n",
		configured, os.Getenv("S3_ENDPOINT"), os.Getenv("S3_REGION"), os.Getenv("S3_BUCKET"), keyset)
}

// handleSetStore writes the device store config to nowhere.conf (root-only, LF -- a CR breaks the agent's
// URL/cred parsing) and applies it to the RUNNING daemon (env + reset the s3 client) so it takes effect
// without a reboot. The su:s0 worker re-sources the conf each run, so it picks the change up automatically.
func handleSetStore(conn net.Conn, ep, rg, bk, ak, sk string) {
	if ep == "" || bk == "" || ak == "" || sk == "" {
		fmt.Fprint(conn, "ERR-FIELDS\n")
		return
	}
	if err := applyStoreConf(ep, rg, bk, ak, sk); err != nil {
		fmt.Fprintf(os.Stderr, "[logind] set-store write: %v\n", err)
		fmt.Fprint(conn, "ERR-WRITE\n")
		return
	}
	fmt.Fprintf(os.Stderr, "[logind] store config updated (%s / %s)\n", ep, bk)
	fmt.Fprint(conn, "OK\n")
}

// applyStoreConf writes the data-store config to nowhere.conf AND applies it to the running daemon (env +
// reset the s3 client) so it takes effect with no reboot. Shared by SET-STORE and discovery.
func applyStoreConf(ep, rg, bk, ak, sk string) error {
	rg = normRegion(rg)
	if err := writeStoreConf(ep, rg, bk, ak, sk); err != nil {
		return err
	}
	os.Setenv("S3_ENDPOINT", ep)
	os.Setenv("S3_REGION", rg)
	os.Setenv("S3_BUCKET", bk)
	os.Setenv("S3_ACCESS_KEY", ak)
	os.Setenv("S3_SECRET_KEY", sk)
	s3client = nil // force s3Init to rebuild the client with the new endpoint/region/creds on the next op
	return nil
}

// autoPublish (re)publishes the LIVE data-store config to the discovery endpoint under (name,pass), so this
// identity re-materializes on another device from name+passphrase alone. Best-effort: a discovery failure
// never breaks create/login. No-op without a discovery endpoint or a data store.
func autoPublish(name, pass string) {
	if !discoConfigured() || !storeConfigured() {
		return
	}
	defer func() { recover() }()
	publishDiscovery("disco", name, pass,
		os.Getenv("S3_ENDPOINT"), os.Getenv("S3_REGION"), os.Getenv("S3_BUCKET"),
		os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"), os.Getenv("GATEWAY_URL"))
	fmt.Fprintf(os.Stderr, "[logind] published discovery for %q\n", name)
}

// tryDiscover bootstraps a NOSTORE device: look up (name,pass)'s data-store config at the discovery
// endpoint and apply it. Returns whether the device is now configured. Best-effort + blind (a wrong
// name/pass just misses). Only runs when there's no store yet and a discovery endpoint is baked.
func tryDiscover(name, pass string) bool {
	if storeConfigured() || !discoCanLookup() { // lookup needs only endpoint+bucket (anonymous if no creds)
		return storeConfigured()
	}
	cfg, ok := func() (s string, found bool) {
		defer func() { recover() }()
		return discoverConfig("disco", name, pass)
	}()
	if !ok {
		return false
	}
	m := map[string]string{}
	for _, ln := range strings.Split(cfg, "\n") {
		if i := strings.IndexByte(ln, '='); i > 0 {
			m[strings.TrimSpace(ln[:i])] = strings.TrimSpace(ln[i+1:])
		}
	}
	if m["S3_ENDPOINT"] == "" || m["S3_BUCKET"] == "" || m["S3_ACCESS_KEY"] == "" || m["S3_SECRET_KEY"] == "" {
		return false
	}
	if applyStoreConf(m["S3_ENDPOINT"], m["S3_REGION"], m["S3_BUCKET"], m["S3_ACCESS_KEY"], m["S3_SECRET_KEY"]) != nil {
		return false
	}
	if gw := m["GATEWAY_URL"]; gw != "" {
		applyGateway(gw) // the billing endpoint roamed with the store config
	}
	fmt.Fprintf(os.Stderr, "[logind] discovered + applied store config for %q\n", name)
	return true
}

// applyGateway sets the billing gateway URL on the running daemon (env, read by os.Getenv("GATEWAY_URL"))
// and persists it to the conf so it survives a reboot. Best-effort persist -- a write failure still leaves
// the URL live for this session.
func applyGateway(url string) {
	os.Setenv("GATEWAY_URL", url)
	if err := upsertConfLine("GATEWAY_URL", url); err != nil {
		fmt.Fprintf(os.Stderr, "[logind] persist GATEWAY_URL: %v\n", err)
	}
}

// upsertConfLine sets key=val in nowhere.conf, replacing any existing line for key and preserving the rest
// (LF, 0600 root). For conf keys outside the S3_* set (e.g. GATEWAY_URL), which writeStoreConf preserves
// but does not itself manage.
func upsertConfLine(key, val string) error {
	var lines []string
	if b, err := os.ReadFile(storeConfPath); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			t := strings.TrimRight(ln, "\r")
			if strings.TrimSpace(t) == "" || strings.HasPrefix(strings.TrimSpace(t), key+"=") {
				continue
			}
			lines = append(lines, t)
		}
	}
	lines = append(lines, key+"="+val)
	os.MkdirAll("/data/nowhere", 0o700)
	return os.WriteFile(storeConfPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// writeStoreConf writes the S3_* lines (LF) and PRESERVES any non-S3 lines (e.g. NOWHERE_SYNC_INTERVAL,
// NOWHERE_DNS) from an existing conf, at 0600 root.
func writeStoreConf(ep, rg, bk, ak, sk string) error {
	var extra []string
	if b, err := os.ReadFile(storeConfPath); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			t := strings.TrimRight(ln, "\r")
			if strings.TrimSpace(t) == "" || strings.HasPrefix(strings.TrimSpace(t), "S3_") {
				continue
			}
			extra = append(extra, t)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "S3_ENDPOINT=%s\nS3_REGION=%s\nS3_BUCKET=%s\nS3_ACCESS_KEY=%s\nS3_SECRET_KEY=%s\n", ep, rg, bk, ak, sk)
	for _, e := range extra {
		sb.WriteString(e)
		sb.WriteString("\n")
	}
	os.MkdirAll("/data/nowhere", 0o700)
	return os.WriteFile(storeConfPath, []byte(sb.String()), 0o600)
}

// handleTestStore validates a store config WITHOUT saving it -- the Settings screen's "Test connection".
// Builds a throwaway minio client with the given creds (DNS resolves via the global NOWHERE_DNS resolver,
// like every other op) and does a lightweight BucketExists, mapping the outcome to a UI-friendly verdict.
func handleTestStore(conn net.Conn, ep, rg, bk, ak, sk string) {
	if ep == "" || bk == "" || ak == "" || sk == "" {
		fmt.Fprint(conn, "ERR-FIELDS\n")
		return
	}
	rg = normRegion(rg)
	secure := !strings.HasPrefix(ep, "http://")
	host := strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	cl, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(ak, sk, ""),
		Secure: secure,
		Region: rg,
	})
	if err != nil {
		fmt.Fprint(conn, "ERR-CONFIG\n") // malformed endpoint, etc.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exists, err := cl.BucketExists(ctx, bk)
	if err != nil {
		er := minio.ToErrorResponse(err)
		if er.Code == "InvalidAccessKeyId" || er.Code == "SignatureDoesNotMatch" || er.StatusCode == 403 {
			fmt.Fprint(conn, "ERR-AUTH\n") // bad access key / secret
		} else {
			fmt.Fprintf(os.Stderr, "[logind] test-store: %v\n", err)
			fmt.Fprint(conn, "ERR-NET\n") // can't reach the endpoint (DNS/TLS/host)
		}
		return
	}
	if !exists {
		fmt.Fprint(conn, "ERR-BUCKET\n") // creds OK but the bucket is missing / not visible
		return
	}
	fmt.Fprint(conn, "OK\n")
}

// handlePingStore reachability-checks the ALREADY-CONFIGURED store for the store screen's connection banner.
// Mirrors handleTestStore but uses the daemon's loaded s3client/s3bucket (no client-supplied creds), so a
// device with a saved store can show live status without re-entering the secret. A bounded BucketExists.
func handlePingStore(conn net.Conn) {
	if !storeConfigured() {
		fmt.Fprint(conn, "ERR-NOSTORE\n")
		return
	}
	s3Init()
	if s3client == nil {
		fmt.Fprint(conn, "ERR-CONFIG\n")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	exists, err := s3client.BucketExists(ctx, s3bucket)
	if err != nil {
		er := minio.ToErrorResponse(err)
		if er.Code == "InvalidAccessKeyId" || er.Code == "SignatureDoesNotMatch" || er.StatusCode == 403 {
			fmt.Fprint(conn, "ERR-AUTH\n") // saved creds no longer valid
		} else {
			fmt.Fprint(conn, "ERR-NET\n") // can't reach the endpoint (DNS/TLS/host/offline)
		}
		return
	}
	if !exists {
		fmt.Fprint(conn, "ERR-BUCKET\n") // creds OK but the bucket is gone / not visible
		return
	}
	fmt.Fprint(conn, "OK\n")
}

// markRoamSession / readRoamSession persist (name, pass, uid) for the live roamed user in the RAM tmpfs
// -- same trick as .session: a plain file, so the sync's push-set skips it and it's gone on power-off.
func markRoamSession(dest, name, pass, uid, seSecret string) {
	// 4th line = se_secret (Endospore hardened identity; empty otherwise). RAM tmpfs only -- gone on
	// power-off, exactly like the passphrase on line 2, so the SE secret is never durable on the device.
	os.WriteFile(filepath.Join(dest, ".roamsession"), []byte(name+"\n"+pass+"\n"+uid+"\n"+seSecret+"\n"), 0o600)
	resetSealStatus() // new session: current as of restore, until a periodic seal actually fails
}

// THROWAWAY = a login whose name has NO head in the store (#4, DIA-20260626-04). It still logs in (an empty
// local session, indistinguishable to an observer -- the blind-login property), but it is LOCAL-ONLY: a
// `.localsession` marker (tmpfs, gone on power-off, like .roamsession) gates every seal OFF, so a throwaway is
// never written to the store and is destroyed on logoff. It still gets the full lock/cold-lock/wipe lifecycle
// (first-class session), and survives cold-lock+resume. PROMOTE ("Save to your store") clears the marker.
func markLocalSession(dest string)  { os.WriteFile(filepath.Join(dest, ".localsession"), []byte("1\n"), 0o600) }
func clearLocalSession(dest string) { os.Remove(filepath.Join(dest, ".localsession")) }
func isLocalSession(dest string) bool {
	_, err := os.Stat(filepath.Join(dest, ".localsession"))
	return err == nil
}

// readRoamSessionSE returns the live session's se_secret (the .roamsession 4th line), or "" when none.
func readRoamSessionSE(dest string) string {
	b, err := os.ReadFile(filepath.Join(dest, ".roamsession"))
	if err != nil {
		return ""
	}
	if p := strings.Split(strings.TrimRight(string(b), "\r\n"), "\n"); len(p) >= 4 {
		return p[3]
	}
	return ""
}

// Backup-health tracking for the chooser's "not backed up" notification (DIA-20260619-12). syncLoop records
// whether each periodic seal pushed cleanly; GET-SYNC-STATUS reports the verdict. "fail" = the last seal
// couldn't reach the store (offline / store down) -> changes since the last good seal are at risk. Reset on
// login (we just restored, so we're current until a seal fails).
var sealMu sync.Mutex
var sealStatus string // "" none-yet/current | "ok" | "fail"

func recordSeal(ok bool) {
	sealMu.Lock()
	if ok {
		sealStatus = "ok"
	} else {
		sealStatus = "fail"
	}
	sealMu.Unlock()
}

func resetSealStatus() { sealMu.Lock(); sealStatus = ""; sealMu.Unlock() }

func sealStatusGet() string { sealMu.Lock(); defer sealMu.Unlock(); return sealStatus }

// ---- managed-store billing wiring (Phase 2, Slice B.5): keep the live session's data leased. ----

func walletPathFor(dest string) string { return filepath.Join(dest, "wallet.json") }

// maybePayRent keeps the live roamed session's data leased on the managed store -- called after each periodic
// seal. Double-gated and best-effort: it runs only when a billing gateway is configured (GATEWAY_URL) AND the
// session has a wallet that holds tokens or a prior lease, and it never blocks or fails the seal loop. payRent
// is idempotent per epoch (a covered epoch costs just one /quote), so calling it every cycle is cheap.
func maybePayRent(dest, name, pass string) {
	gw := os.Getenv("GATEWAY_URL")
	if gw == "" {
		return // managed-store billing not configured on this device
	}
	defer func() { recover() }() // billing must never crash the seal loop
	wpath := walletPathFor(dest)
	w, err := loadWallet(wpath)
	if err != nil {
		return
	}
	// A paid session has wallet balance or a recorded lease; a FREE session (#54) has neither but still needs
	// its free lease renewed, so in cap (managed) mode proceed and let payRent renew + throttle (and no-op a
	// throwaway whose footprint is empty). Outside cap mode, keep skipping never-engaged sessions.
	if w.balance() == 0 && w.PaidThrough == 0 && !capMode() {
		return // no wallet / never engaged the managed store -> nothing to lease
	}
	info, paid, err := payRent(newGatewayClient(gw), "s3", name, pass, wpath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[logind] pay-rent %q: %v\n", name, err)
		return
	}
	if paid {
		fmt.Fprintf(os.Stderr, "[logind] pay-rent %q: paid through epoch %d (%d token(s) spent)\n", name, info.ThroughEpoch, info.Spent)
	}
}

// sealWalletBestEffort roams the live session's token wallet to the store if present, so mint/spend changes
// survive the power-off wipe even when no lease re-sealed it this session. Best-effort, recover-guarded.
func sealWalletBestEffort(dest, name, pass string) {
	defer func() { recover() }()
	wpath := walletPathFor(dest)
	if _, err := os.Stat(wpath); err != nil {
		return // no wallet this session
	}
	if dk := resolveDK("s3", name, pass); dk != nil {
		sealWallet("s3", name, pass, dk, wpath)
	}
}

// syncLoop continuously seals the LIVE roamed user's data to the store so an unclean power-off (battery,
// crash) loses at most one interval instead of the whole session -- the store is the only durable copy
// (/data is wiped on power-off). It reuses the worker's "out" op (push /data/user/N + user_de + media)
// WITHOUT the logoff reboot, serialized with login/logout via workerMu (inside triggerRoamWorker).
// Convergent sealing dedups unchanged data, so an idle cycle uploads nothing. No live session => skip.
// Supersedes the Arc-1 diaspore_sync.sh, which keyed off the tmpfs .session marker + pushed the
// working-set dir -- both wrong for the Arc-2 per-user model (it skipped every cycle).
func syncLoop(dest string) {
	for {
		// Re-read each cycle so a gate -> Settings change to the cadence applies live, without a reboot.
		time.Sleep(time.Duration(syncIntervalGet()) * time.Second)
		name, pass, uid := readRoamSession(dest)
		if name == "" || uid == "" {
			continue // nobody logged in -> nothing to seal
		}
		if isLocalSession(dest) {
			continue // #4: throwaway -> LOCAL-ONLY, never sealed to the store (until PROMOTE)
		}
		out := triggerRoamWorker(dest, "out", name, pass, uid, readRoamSessionSE(dest), nil) // seal only -- no reboot, no marker clear
		if out == "SKIP-REAPING" {
			continue // a reap is in progress for this user -> skip silently (not a seal failure)
		}
		recordSeal(out == "OK")                                     // drive the chooser's "not backed up" notification
		fmt.Fprintf(os.Stderr, "[logind] periodic seal %q user %s -> %s\n", name, uid, out)
		maybePayRent(dest, name, pass) // keep the managed-store lease current (best-effort; no-op when not configured)
		if gw := os.Getenv("GATEWAY_URL"); gw != "" {
			maybeRefill(newGatewayClient(gw), capWalletPath()) // subscription auto-refill, once per billing epoch
		}
	}
}

func readRoamSession(dest string) (name, pass, uid string) {
	b, err := os.ReadFile(filepath.Join(dest, ".roamsession"))
	if err != nil {
		return "", "", ""
	}
	p := strings.SplitN(strings.TrimRight(string(b), "\r\n"), "\n", 4) // 4 = tolerate the optional se_secret line
	for len(p) < 3 {
		p = append(p, "")
	}
	return p[0], p[1], p[2]
}

// handleGetUsage answers the Profile screen's GB-USED line: the live profile's footprint in the store.
// Reuses the session creds from .roamsession (same source the periodic seal uses) to resolve the profile's
// refs + sizes via profileFootprint -- a store round-trip, so the screen loads it async. "NONE" when no one
// is logged in, "LOCAL" for a throwaway (not in the store), else "OK bytes=<total>".
func handleGetUsage(conn net.Conn, dest string) {
	name, pass, _ := readRoamSession(dest)
	if name == "" {
		fmt.Fprint(conn, "NONE\n")
		return
	}
	if isLocalSession(dest) {
		fmt.Fprint(conn, "LOCAL\n")
		return
	}
	var total int64
	for _, sz := range profileFootprint("s3", name, pass) {
		total += sz
	}
	fmt.Fprintf(conn, "OK bytes=%d\n", total)
}

// handleBackup forces an immediate seal of the LIVE roamed session to the store -- the "Back up now" action on
// the Profile app's Your-data screen (DIA-20260628-09). It runs the SAME seal the periodic syncLoop runs, just
// on demand, and reuses the logoff progress channel (sealInFlight/sealProg) so the screen can poll SEAL-STATUS
// for a "Backing up… N%" line. Reply-then-seal-in-a-goroutine (not inline) so the single-threaded daemon stays
// free to serve those polls -- the same shape as handleLogout, minus the reap (we are NOT logging off).
// handleClaim is the "Add credits" daemon path: drain a paid claim code into the live session's wallet
// (zero-knowledge blind tokens). Works for a STORED profile (a top-up) AND a throwaway -- a throwaway's
// credit goes into its local wallet and rides the promote when the user saves it to the store (the
// throwaway -> Save-to-store flow). Needs a live session + a configured gateway. New credit is spendable
// at once; the chooser follows a STORED top-up with a BACKUP so it also roams.
func handleClaim(conn net.Conn, dest, code string) {
	gw := strings.TrimRight(os.Getenv("GATEWAY_URL"), "/")
	if gw == "" {
		fmt.Fprint(conn, "ERR\n") // no managed gateway configured -> nothing to claim against
		return
	}
	if code == "" {
		fmt.Fprint(conn, "ERR-NOCODE\n")
		return
	}
	if _, err := os.Stat(filepath.Join(dest, ".roamsession")); err != nil {
		fmt.Fprint(conn, "NONE\n") // no live session -> no wallet to credit
		return
	}
	n, err := drainClaim(newGatewayClient(gw), capWalletPath(), code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[logind] claim -> %v\n", err)
		fmt.Fprint(conn, "ERR\n")
		return
	}
	if n == 0 {
		fmt.Fprint(conn, "ERR-NOCODE\n") // unknown code or already fully spent
		return
	}
	fmt.Fprintf(os.Stderr, "[logind] add-credits: +%d token(s)\n", n)
	fmt.Fprintf(conn, "OK %d\n", n)
}

// handleSubscribe is the "Add credits -> subscription" daemon path: store the bearer subkey in the live
// session's roaming wallet + do an immediate first refill. Like handleClaim, needs a STORED session (a
// throwaway has no roaming wallet) + a gateway. n==0 is success too (subkey stored; this epoch isn't credited
// yet -> maybeRefill picks it up later). The chooser follows OK with a BACKUP so the subkey roams.
func handleSubscribe(conn net.Conn, dest, subkey string) {
	gw := strings.TrimRight(os.Getenv("GATEWAY_URL"), "/")
	if gw == "" {
		fmt.Fprint(conn, "ERR\n")
		return
	}
	if subkey == "" {
		fmt.Fprint(conn, "ERR-NOCODE\n")
		return
	}
	if _, err := os.Stat(filepath.Join(dest, ".roamsession")); err != nil {
		fmt.Fprint(conn, "NONE\n") // no live session -> no wallet to subscribe
		return
	}
	if isLocalSession(dest) {
		fmt.Fprint(conn, "LOCAL\n") // a throwaway has no roaming wallet
		return
	}
	n, err := subscribe(newGatewayClient(gw), capWalletPath(), subkey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[logind] subscribe -> %v\n", err)
		fmt.Fprint(conn, "ERR\n")
		return
	}
	fmt.Fprintf(os.Stderr, "[logind] subscribe: stored subkey, +%d token(s) this epoch\n", n)
	fmt.Fprintf(conn, "OK %d\n", n)
}

func handleBackup(conn net.Conn, dest string) {
	name, pass, uid := readRoamSession(dest)
	if name == "" || uid == "" {
		fmt.Fprint(conn, "NONE\n") // nobody logged in -> nothing to back up
		return
	}
	if isLocalSession(dest) {
		fmt.Fprint(conn, "LOCAL\n") // a throwaway lives only on this phone -> never sealed to the store
		return
	}
	pendingReapMu.Lock()
	busy := sealInFlight
	if !busy {
		sealInFlight = true // claim the progress channel BEFORE replying so the screen's first SEAL-STATUS poll
		sealProg = ""       // can't race a not-yet-started seal and read a false "DONE"
	}
	pendingReapMu.Unlock()
	if busy {
		fmt.Fprint(conn, "BUSY\n") // a logout/periodic seal already holds the channel -> let that one finish
		return
	}
	seSecret := readRoamSessionSE(dest)
	fmt.Fprint(conn, "OK\n")
	go func() {
		time.Sleep(500 * time.Millisecond)
		syscall.Sync() // flush lazily-persisted app state (SharedPreferences/SQLite) before sealing (cf. logout)
		out := triggerRoamWorkerKind(dest, "out", name, pass, uid, seSecret, "manual", func(p string) { // #58: a pinned manual save-point
			pendingReapMu.Lock()
			sealProg = p
			pendingReapMu.Unlock()
		})
		pendingReapMu.Lock()
		sealInFlight = false
		pendingReapMu.Unlock()
		recordSeal(out == "OK")
		maybePayRent(dest, name, pass) // keep the managed-store lease current (best-effort; no-op when unconfigured)
		fmt.Fprintf(os.Stderr, "[logind] manual backup %q user %s -> %s\n", name, uid, out)
	}()
}

// handleExport seals an export bundle (a zip of the session's vCard/iCal, built by the chooser, which holds the
// contacts/calendar read perms) under the live profile's key and stores it at ref export/<name> -- the
// zero-knowledge "take your data with you" target (DIA-20260628-09 P2b). The store sees only ciphertext; the web
// storefront later re-derives the key from name+passphrase to fetch + decrypt it. A roamed session is a secondary
// Android user with no usable shared storage / MTP, so the store is the export's destination, not a local file.
func handleExport(conn net.Conn, dest string, bundle []byte) {
	name, pass, _ := readRoamSession(dest)
	if name == "" {
		fmt.Fprint(conn, "NONE\n") // nobody logged in
		return
	}
	if isLocalSession(dest) {
		fmt.Fprint(conn, "LOCAL\n") // a throwaway has no store to seal into
		return
	}
	dk := resolveDK("s3", name, pass)
	if dk == nil {
		fmt.Fprint(conn, "ERR-KEY\n")
		return
	}
	var blob string
	werr := func() (e error) {
		defer func() {
			if r := recover(); r != nil {
				e = fmt.Errorf("%v", r) // postBlob/putRef/capFlush fail() by panic on a persistent store error
			}
		}()
		sealed := seal(dk, bundle) // nonce || AES-256-GCM(bundle)
		// Managed mode holds no store creds, so the blob + ref ride a cap lease (capWrite brackets them and
		// pays from the wallet); self-hosted writes directly. Without the bracket the direct postBlob failed
		// "outside a capBegin/capFlush bracket" -- export never worked on a managed device before DIA-20260702-04.
		capWrite(name, pass, dk, func() {
			blob = postBlob("s3", sealed)      // blob/<sha256(sealed)>; buffered in managed mode
			putRef("s3", "export/"+name, blob) // ref/export/<name> -> the blob hash
		})
		return nil
	}()
	if werr != nil {
		fmt.Fprintf(os.Stderr, "[logind] export %q: %v\n", name, werr)
		fmt.Fprint(conn, "ERR-STORE\n")
		return
	}
	fmt.Fprintf(os.Stderr, "[logind] export %q: sealed %d bytes -> ref/export/%s (blob %s)\n",
		name, len(bundle), name, blob[:12])
	fmt.Fprintf(conn, "OK bytes=%d\n", len(bundle))
}

// handleGetSnapshots (#58) lists the live session's retained rollback snapshots for the "Restore a snapshot"
// screen: "SNAP <version> <unixTime>" per snapshot (newest-first) terminated by "END", or NONE/LOCAL/EMPTY.
func handleGetSnapshots(conn net.Conn, dest string) {
	name, pass, _ := readRoamSession(dest)
	if name == "" {
		fmt.Fprint(conn, "NONE\n") // nobody logged in
		return
	}
	if isLocalSession(dest) {
		fmt.Fprint(conn, "LOCAL\n") // a throwaway never roamed -> no store snapshots
		return
	}
	snaps := snapshotList("s3", name, pass)
	if len(snaps) == 0 {
		fmt.Fprint(conn, "EMPTY\n") // stored, but no prior versions yet
		return
	}
	for _, s := range snaps {
		kind := s.Kind
		if kind == "" {
			kind = "auto"
		}
		fmt.Fprintf(conn, "SNAP %d %d %s\n", s.Version, s.Time, kind) // #58: kind = manual (pinned) | auto
	}
	fmt.Fprint(conn, "END\n")
}

// handleRollback (#58) rolls the live session's STORE head back to a retained snapshot, then tears the session
// down WITHOUT sealing (the throwaway-logout path) so the current local data can't be re-sealed over the
// snapshot. The user-0 watcher then switches to the gate + removes the user; a fresh login restores the
// rolled-back head. Reply "OK <newVersion>" | NONE | LOCAL | ERR.
func handleRollback(conn net.Conn, dest, verStr string) {
	name, pass, uid := readRoamSession(dest)
	if name == "" || uid == "" {
		fmt.Fprint(conn, "NONE\n")
		return
	}
	if isLocalSession(dest) {
		fmt.Fprint(conn, "LOCAL\n") // a throwaway has no store head / snapshots
		return
	}
	var ver uint64
	if verStr != "" {
		ver, _ = strconv.ParseUint(verStr, 10, 64)
	}
	seSecret := readRoamSessionSE(dest) // capture before clearing markers (hardened vaults)
	// Stop the periodic sync from racing a seal of the CURRENT data over the snapshot: drop .roamsession FIRST,
	// then roll the head. On failure, re-arm the session so it stays sync'd and the user isn't stranded.
	os.Remove(filepath.Join(dest, ".roamsession"))
	nv, err := rollbackHead("s3", name, pass, ver)
	if err != nil {
		markRoamSession(dest, name, pass, uid, seSecret) // re-arm: nothing was torn down
		fmt.Fprintf(os.Stderr, "[logind] rollback %q user %s FAILED: %v\n", name, uid, err)
		if errors.Is(err, errNoSnapshot) {
			fmt.Fprint(conn, "NOSNAP\n") // stale list -> tell the user to pick again, not to check their connection
		} else {
			fmt.Fprint(conn, "ERR\n")
		}
		return
	}
	// Head rolled. Discard the current session WITHOUT sealing (same teardown as a throwaway logout) and queue
	// the switch+reap for the user-0 watcher.
	clearLocalSession(dest)
	pendingReapMu.Lock()
	pendingSwitchUID = uid
	pendingReapUID = uid
	pendingReapMu.Unlock()
	fmt.Fprintf(os.Stderr, "[logind] rollback %q user %s -> head v%d; reap WITHOUT seal (re-login to load it)\n", name, uid, nv)
	fmt.Fprintf(conn, "OK %d\n", nv)
}

// handleRollbackGate (#58 P4) rolls a profile's store head to its NEWEST snapshot from the GATE -- used when a
// login restore fails (corrupt chunk) and the user opts to fall back. No live session exists (login failed), so
// there's nothing to reap; a retry login then restores the rolled-back head. Reply "OK <ver>" | NOSNAP | ERR.
func handleRollbackGate(conn net.Conn, name, pass string) {
	if name == "" || pass == "" {
		fmt.Fprint(conn, "ERR\n")
		return
	}
	nv, err := rollbackHead("s3", name, pass, 0) // 0 = the newest retained snapshot (the last good head)
	if err != nil {
		if errors.Is(err, errNoSnapshot) {
			fmt.Fprint(conn, "NOSNAP\n") // no earlier version to fall back to
			return
		}
		fmt.Fprintf(os.Stderr, "[logind] rollback-gate %q FAILED: %v\n", name, err)
		fmt.Fprint(conn, "ERR\n")
		return
	}
	fmt.Fprintf(os.Stderr, "[logind] rollback-gate %q -> head v%d (last good snapshot)\n", name, nv)
	fmt.Fprintf(conn, "OK %d\n", nv)
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "ERR:", r)
			os.Exit(1)
		}
	}()
	// Android provides no /etc/resolv.conf and empty net.dns*, so Go's pure (cgo-less) resolver falls back
	// to localhost:53 and every lookup fails. installResolver points net.DefaultResolver at a real
	// nameserver -- preferring the device's own per-network DNS, an explicit NOWHERE_DNS override, or a
	// public fallback -- and is a no-op on a normal host where resolv.conf works. See resolver.go.
	installResolver()
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "get": // get <url>
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(os.Args[2])
		check(err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		fmt.Printf("OK status=%d body=%q\n", resp.StatusCode, string(b))

	case "ota-mark": // ota-mark <store> <version>   (build host: record the latest published OS version)
		putRef(os.Args[2], otaVersionRef(), os.Args[3])
		fmt.Printf("ota-mark: %s = %s\n", otaVersionRef(), os.Args[3])

	case "ota-check": // ota-check <store> [runningVersionFile]   (device: exit 0 if a newer OS is published)
		runFile := ""
		if len(os.Args) > 3 {
			runFile = os.Args[3]
		}
		latest, avail := otaCheck(os.Args[2], runFile)
		if latest == "" {
			fmt.Println("ota-check: no published OS version")
			os.Exit(3)
		}
		if avail {
			fmt.Printf("ota-check: UPDATE -> %s\n", latest)
			os.Exit(0) // newer OS available
		}
		fmt.Printf("ota-check: UPTODATE (latest %s)\n", latest)
		os.Exit(1) // nothing newer

	case "restore": // restore <store> <profile> <pass> <destDir>  (CDC manifest; legacy single-blob fallback)
		base, profile, pass, dest := os.Args[2], os.Args[3], os.Args[4], os.Args[5]
		key, manifestBlob, ok := resolveKey(base, profile, pass)
		if !ok {
			fmt.Printf("restore: profile %q empty -> nothing\n", profile)
			return
		}
		pt := unseal(key, manifestBlob)
		os.MkdirAll(dest, 0o700)
		if bytes.HasPrefix(pt, cdcMagic) {
			var m chunkManifest
			check(json.Unmarshal(pt[len(cdcMagic):], &m))
			// Fetch the chunks concurrently (bounded window) but write them in order into the untar
			// (DIA-20260624-07). NOWHERE_PHASE (apps|secure|media), set per call by the roam worker, drives the
			// gate's restore bar via writeProg inside cdcRestore; no-op when the env isn't set (CLI/host).
			cdcRestore(base, key, m, dest)
			if chunkCacheDir != "" {
				fmt.Printf("restore: profile %q (%d chunks: %d cached, %d fetched) -> %s\n", profile, len(m.Chunks), atomic.LoadInt64(&chunkHits), atomic.LoadInt64(&chunkMiss), dest)
			} else {
				fmt.Printf("restore: profile %q (%d chunks) -> %s\n", profile, len(m.Chunks), dest)
			}
		} else {
			untarFrom(dest, bytes.NewReader(pt)) // pre-CDC ref: the blob IS the tar
			fmt.Printf("restore: profile %q (legacy %d B) -> %s\n", profile, len(pt), dest)
		}
		// #72: reached ONLY on a fully-completed restore -- cdcRestore/untarFrom fail-hard (panic -> non-zero
		// exit) on any chunk error, so this line is the byte-complete-copy proof. Record the receipt so a later
		// seal of this ref is permitted; a failed/partial restore never reaches here and leaves no receipt, so
		// its seal is refused and the good head survives. Keyed by the STABLE profileRefV2 (#80), uniform across
		// the CE/#de/#media phases (each is its own `restore` call), so a seal that migrates the head legacy->v2
		// still finds this receipt (headKey would flip and orphan it).
		writeRestoreReceipt(profileRefV2(profile, pass), getRef(base, headKey(base, profile, pass)))

	case "export-fetch": // export-fetch <store> <name> <pass> <destZip>   (P2b verify / storefront reference:
		// fetch ref/export/<name> -> blob -> unseal under the profile DK -> the zip bundle the user downloads)
		base, name, pass, out := os.Args[2], os.Args[3], os.Args[4], os.Args[5]
		dk := resolveDK(base, name, pass)
		if dk == nil {
			fmt.Printf("export-fetch: profile %q not found or wrong pass\n", name)
			os.Exit(1)
		}
		blob := getRef(base, "export/"+name)
		if blob == "" {
			fmt.Printf("export-fetch: no export for %q\n", name)
			os.Exit(2)
		}
		pt := unseal(dk, getBlob(base, blob))
		check(os.WriteFile(out, pt, 0o600))
		fmt.Printf("export-fetch: %q export/%s -> %s (%d bytes)\n", name, name, out, len(pt))

	case "push": // push <store> <profile> <pass> <srcDir>  (CDC; updates the vault in place if the profile has one)
		pushProfile(os.Args[2], os.Args[3], os.Args[4], os.Args[5])

	case "push-set": // push-set <store> <profile> <pass> <srcRoot>   (subdirs "N-name" -> items)
		base, profile, pass, root := os.Args[2], os.Args[3], os.Args[4], os.Args[5]
		n := pushSet(base, profile, pass, root)
		fmt.Printf("push-set: profile %q manifest with %d items\n", profile, n)

	case "restore-set": // restore-set <store> <profile> <pass> <destRoot> <maxPrio>
		base, profile, pass, dest := os.Args[2], os.Args[3], os.Args[4], os.Args[5]
		maxPrio, _ := strconv.Atoi(os.Args[6])
		restoreSet(base, profile, pass, dest, maxPrio, true)

	case "migrate-ref": // migrate-ref <store> <name> <pass>   (one-time: name-only ref -> name+pass ref)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		oh := sha256.Sum256([]byte("nowhere-ref:" + name)) // the OLD name-only scheme
		head := getRef(base, hex.EncodeToString(oh[:]))
		if head == "" {
			fmt.Printf("migrate-ref: no old ref for %q\n", name)
			return
		}
		putHead(base, name, pass, head) // re-point only; content-addressed blobs are unchanged
		fmt.Printf("migrate-ref: %q -> name+pass ref now points to %s\n", name, short(head))

	case "create-vault": // create-vault <store> <name> <pass>   (new profile in the vault model; prints the 12-word code)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		if getRef(base, headKey(base, name, pass)) != "" {
			fail("create-vault: profile already exists")
		}
		fmt.Printf("RECOVERY %s\n", createVault(base, name, pass))

	case "enroll-se": // enroll-se <store> <name> <pass> <se_secret_hex>  (Endospore E.3b: harden to this device's SE)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		sb, err := hex.DecodeString(os.Args[5])
		if err != nil {
			fail("enroll-se: se_secret must be hex")
		}
		if err := enrollSE(base, name, pass, sb); err != nil {
			fail("enroll-se: " + err.Error())
		}
		fmt.Println("enrolled: hardened (pass-only slot dropped; open via SE device or recovery)")

	case "migrate-vault": // migrate-vault <store> <name> <pass>   (upgrade a legacy profile to a vault; prints the code)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		if getRef(base, headKey(base, name, pass)) == "" {
			fail("migrate-vault: wrong passphrase / no profile")
		}
		if m, migrated := migrateVault(base, name, pass); migrated {
			fmt.Printf("RECOVERY %s\n", m)
		} else {
			fmt.Printf("migrate-vault: %q already a vault\n", name)
		}

	case "rotate": // rotate <store> <name> <oldpass> <newpass>   (change passphrase; no data re-encryption)
		base, name, oldpass, newpass := os.Args[2], os.Args[3], os.Args[4], os.Args[5]
		if err := rotateVault(base, name, oldpass, newpass); err != nil {
			fail("rotate: " + err.Error())
		}
		fmt.Printf("rotate: %q passphrase changed\n", name)

	case "recover": // recover <store> <name> <mnemonic> <newpass>   (reset the passphrase via the 12-word code)
		base, name, mnemonic, newpass := os.Args[2], os.Args[3], os.Args[4], os.Args[5]
		if err := recoverVault(base, name, mnemonic, newpass); err != nil {
			fail("recover: " + err.Error())
		}
		fmt.Printf("recover: %q reset to a new passphrase\n", name)

	case "snapshots": // snapshots <store> <name> <pass>   (#58: list retained rollback snapshots, newest-first)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		for _, s := range snapshotList(base, name, pass) {
			fmt.Printf("v%d\t%s\t%s\n", s.Version, time.Unix(s.Time, 0).UTC().Format(time.RFC3339), short(s.Manifest))
		}

	case "rollback-head": // rollback-head <store> <name> <pass> [version]   (#58: repoint the head to a retained snapshot; 0/absent = newest good)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		var ver uint64
		if len(os.Args) > 5 {
			ver, _ = strconv.ParseUint(os.Args[5], 10, 64)
		}
		nv, err := rollbackHead(base, name, pass, ver)
		if err != nil {
			fail("rollback-head: " + err.Error())
		}
		fmt.Printf("rollback-head: %q -> new head v%d (rolled to a retained snapshot)\n", name, nv)

	case "delete-profile": // delete-profile <store> <name> <pass>   (drop the head + recovery refs; blobs are left for store GC)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		if deleteProfile(base, name, pass) {
			fmt.Printf("delete-profile: %q removed\n", name)
		} else {
			fmt.Printf("delete-profile: %q not found / wrong passphrase\n", name)
		}

	case "publish-discovery": // publish-discovery <discoStore> <name> <pass> <endpoint> <region> <bucket> <key> <secret> [gatewayURL]
		ds, n, p := os.Args[2], os.Args[3], os.Args[4]
		gw := ""
		if len(os.Args) > 10 {
			gw = os.Args[10]
		}
		publishDiscovery(ds, n, p, os.Args[5], os.Args[6], os.Args[7], os.Args[8], os.Args[9], gw)
		fmt.Printf("publish-discovery: %q published (ref %s…)\n", n, short(bootstrapRef(n, p)))

	case "set-gateway": // set-gateway <url>   (set + persist the billing gateway URL in nowhere.conf; "" clears)
		applyGateway(os.Args[2])
		fmt.Printf("set-gateway: GATEWAY_URL=%s\n", os.Args[2])

	case "discover": // discover <discoStore> <name> <pass>   (fetch + unseal this identity's store-config)
		ds, n, p := os.Args[2], os.Args[3], os.Args[4]
		if cfg, ok := discoverConfig(ds, n, p); ok {
			fmt.Print(cfg) // the S3_* conf lines
		} else {
			fmt.Println("NOTFOUND")
		}

	case "wallet": // wallet <walletPath>   (show the token balance)
		w, err := loadWallet(os.Args[2])
		check(err)
		fmt.Printf("wallet: %d token(s) (%d zero-knowledge, %d faucet)\n", w.balance(), len(w.BlindTokens), len(w.Tokens))

	case "wallet-buy": // wallet-buy <gatewayURL> <walletPath> <count>   (dev: mint faucet tokens into the wallet)
		count, _ := strconv.Atoi(os.Args[4])
		toks, err := newGatewayClient(os.Args[2]).buyFaucet(count)
		check(err)
		w, err := loadWallet(os.Args[3])
		check(err)
		w.Tokens = append(w.Tokens, toks...)
		check(saveWallet(os.Args[3], w))
		fmt.Printf("wallet-buy: +%d token(s) -> %d total\n", len(toks), w.balance())

	case "wallet-claim": // wallet-claim <gatewayURL> <walletPath> <claim> [count]   (redeem a paid claim code; omit count to drain the whole claim)
		gw := newGatewayClient(os.Args[2])
		var n int
		var err error
		if len(os.Args) > 5 {
			count, _ := strconv.Atoi(os.Args[5])
			n, err = claimTokens(gw, os.Args[3], os.Args[4], count)
		} else {
			n, err = drainClaim(gw, os.Args[3], os.Args[4]) // device path: claim the entire code, count unknown
		}
		check(err)
		w, err := loadWallet(os.Args[3])
		check(err)
		fmt.Printf("wallet-claim: +%d zero-knowledge token(s) -> %d total\n", n, w.balance())

	case "wallet-subscribe": // wallet-subscribe <gatewayURL> <walletPath> <subkey>   (store a subscription secret + refill this epoch)
		n, err := subscribe(newGatewayClient(os.Args[2]), os.Args[3], os.Args[4])
		check(err)
		w, err := loadWallet(os.Args[3])
		check(err)
		fmt.Printf("wallet-subscribe: stored subkey, +%d token(s) this epoch -> %d total\n", n, w.balance())

	case "lease": // lease <gatewayURL> <walletPath> <store> <profile> <pass>   (pay rent to keep the profile's data alive)
		refs := profileFootprint(os.Args[4], os.Args[5], os.Args[6])
		if len(refs) == 0 {
			fmt.Printf("lease: profile %q empty -> nothing to lease\n", os.Args[5])
			return
		}
		info, err := leaseRefs(newGatewayClient(os.Args[2]), os.Args[3], refs)
		if err != nil {
			if errors.Is(err, errInsufficientCredit) {
				fmt.Println("lease: insufficient credit -- top up the wallet")
				os.Exit(1)
			}
			check(err)
		}
		fmt.Printf("lease: %q paid through epoch %d (%d token(s) spent, %d ref(s))\n",
			os.Args[5], info.ThroughEpoch, info.Spent, len(refs))

	case "wallet-pull": // wallet-pull <store> <profile> <pass> <walletPath>   (restore the roaming wallet at login)
		dk := resolveDK(os.Args[2], os.Args[3], os.Args[4])
		if dk == nil {
			fmt.Println("wallet-pull: unknown profile or wrong passphrase")
			os.Exit(1)
		}
		check(restoreWallet(os.Args[2], os.Args[3], os.Args[4], dk, os.Args[5]))
		w, err := loadWallet(os.Args[5])
		check(err)
		fmt.Printf("wallet-pull: %d token(s) restored (paid through epoch %d)\n", w.balance(), w.PaidThrough)

	case "wallet-push": // wallet-push <store> <profile> <pass> <walletPath>   (seal the wallet to the store WITHOUT leasing -- seed/provision)
		dk := resolveDK(os.Args[2], os.Args[3], os.Args[4])
		if dk == nil {
			fmt.Println("wallet-push: unknown profile or wrong passphrase")
			os.Exit(1)
		}
		h, err := sealWallet(os.Args[2], os.Args[3], os.Args[4], dk, os.Args[5])
		check(err)
		if h == "" {
			fmt.Println("wallet-push: no wallet file to seal")
			return
		}
		w, _ := loadWallet(os.Args[5])
		fmt.Printf("wallet-push: sealed %d token(s) (paid through epoch %d) -> %s…\n", w.balance(), w.PaidThrough, short(h))

	case "pay-rent": // pay-rent <gatewayURL> <store> <profile> <pass> <walletPath>   (roaming wallet + lease; idempotent per epoch)
		info, paid, err := payRent(newGatewayClient(os.Args[2]), os.Args[3], os.Args[4], os.Args[5], os.Args[6])
		if err != nil {
			if errors.Is(err, errInsufficientCredit) {
				fmt.Println("pay-rent: insufficient credit -- top up the wallet")
				os.Exit(1)
			}
			check(err)
		}
		if !paid {
			fmt.Printf("pay-rent: %q already covered -- nothing due\n", os.Args[4])
			return
		}
		fmt.Printf("pay-rent: %q paid through epoch %d (%d token(s) spent)\n", os.Args[4], info.ThroughEpoch, info.Spent)

	case "login-daemon": // login-daemon  (root side of the blind-login chooser; AF_UNIX socket from init)
		loginDaemon()

	default:
		usage()
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// progPhase, when non-empty, OVERRIDES NOWHERE_PHASE for writeProg. The seal ("out") worker uses it to
// switch the gate's logoff label between its two real phases WITHIN one process -- "prepare" while it
// scans+chunks+dedups the session (buffering new blobs), then "save" while capFlush uploads them -- so the
// user sees "Preparing your data… X%" then "Saving your data… X%" instead of one number-less "Saving…"
// (DIA-20260701-01). Empty = fall back to the env/default (restore leaves it unset -> uses apps|secure|media).
var progPhase atomic.Value // string

func setProgPhase(p string) { progPhase.Store(p) }

// writeProg reports restore progress to the file named by NOWHERE_PROGRESS as one line
// "<phase> <done> <total>", atomically (temp+rename) so a concurrent reader -- the login daemon, which
// streams it to the gate's restore bar -- never sees a torn write. NOWHERE_PHASE labels which class of
// data this restore is (apps|secure|media); progPhase overrides it for the seal's prepare/save split.
// No-op when NOWHERE_PROGRESS isn't set (CLI / host use).
func writeProg(done, total int) {
	file := os.Getenv("NOWHERE_PROGRESS")
	if file == "" || total <= 0 { // skip empty pushes (an empty DE/media seal) so the bar HOLDS at the prior 100% instead of resetting to 0% between the CE/DE/media pushes (DIA-20260625-04)
		return
	}
	phase := os.Getenv("NOWHERE_PHASE")
	if o, _ := progPhase.Load().(string); o != "" {
		phase = o
	}
	if phase == "" {
		phase = "data"
	}
	tmp := file + ".tmp"
	if os.WriteFile(tmp, []byte(fmt.Sprintf("%s %d %d\n", phase, done, total)), 0o600) == nil {
		os.Rename(tmp, file)
	}
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
func fail(msg string) { panic(errors.New(msg)) }
func usage() {
	fmt.Println("usage: nowhere_agent <get|restore|push|push-set|restore-set|create-vault|migrate-vault|rotate|recover|delete-profile|ota-mark|ota-check|publish-discovery|discover|login-daemon> ...")
	os.Exit(2)
}
