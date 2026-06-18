// Diaspore on-device roaming agent (Phase 2: P2.2 on-device, P2.3 blind-login, P2.5 lazy).
//
// Static Go binary that runs ON the device over its own network: HTTP to the content-addressed
// store + AES-256-GCM + tar. (Also runs on the host for key/manifest model tests; logic is identical.)
//
// P2.3 blind-login keys:  ref = sha256("diaspore-ref:"+name) ; key = Argon2id(pass, sha256("diaspore-salt:"+name)).
// P2.5 lazy restore:      the profile head points at a MANIFEST of items (each its own sealed,
//   content-addressed blob) with priorities. restore-set <maxPrio> restores only items at/under
//   that priority, so the WORKING SET comes back first (device usable) and bulk is deferred.
// P3.1 login-daemon:      the root side of the blind-login chooser. init hands us an AF_UNIX socket;
//   the chooser app connects and sends "<name>\n<pass>\n"; we restore that profile's working set into
//   /data/diaspore/state IN-PROCESS (passphrase only ever in memory, never on disk or in argv).
//
// Stores (selected by the <store> arg):
//   - an HTTP store (Phase 0 store_server.py):  POST /blob -> hash ; GET /blob/<hash> ; GET|PUT /ref/<name>
//   - "s3"  -> any S3-compatible store (Filebase/Sia, Cloudflare R2, Backblaze B2, MinIO). P4.2b.
//             config via env: S3_ENDPOINT S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET [S3_REGION].
//             blobs -> object blob/<hash> (immutable) ; head -> object ref/<name> (mutable).
// Build:  CGO_ENABLED=0 go build -o diaspore_agent .
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
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/argon2"
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
func profileRef(name, passphrase string) string {
	h := sha256.Sum256([]byte("diaspore-ref:" + name + "\x00" + passphrase))
	return hex.EncodeToString(h[:])
}

func deriveKey(name, passphrase string) []byte {
	salt := sha256.Sum256([]byte("diaspore-salt:" + name))
	return argon2.IDKey([]byte(passphrase), salt[:], 1, 64*1024, 4, 32)
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

type vault struct {
	V           int       `json:"v"`
	Slots       []keyslot `json:"slots"`
	Manifest    string    `json:"manifest"`     // content-hash of the DK-sealed CDC chunk-manifest (the data head)
	RecoveryRef string    `json:"recovery_ref"` // f(name,entropy) -- stored so push keeps it in sync (a hash; leaks nothing)
	Version     uint64    `json:"version"`      // monotonic head version: bumped on every push/rotate -> rollback signal
	Sig         string    `json:"sig"`          // base64 HMAC over the header (Sig cleared), keyed by the DK -> tamper-evidence
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
	salt := sha256.Sum256([]byte("diaspore-recovery-salt:" + name))
	return argon2.IDKey(entropy, salt[:], 1, 64*1024, 4, 32)
}

// recoveryRef is the unguessable store location for the vault via the recovery code -- the parallel of
// profileRef for the recovery path. A hash, so storing it in the vault leaks nothing.
func recoveryRef(name string, entropy []byte) string {
	h := sha256.Sum256([]byte("diaspore-recovery-ref:" + name + "\x00" + hex.EncodeToString(entropy)))
	return hex.EncodeToString(h[:])
}

// ---- Discovery / bootstrap (Tier 2 target): name+passphrase -> the device's STORE config, so a profile
// re-materializes on any Diaspore device without re-entering store creds. At enrollment we seal the
// store-config under a (name,pass)-derived key and PUT it at an unguessable bootstrapRef on a baked
// DISCOVERY endpoint; a fresh device derives the same ref, GETs the ciphertext, unseals, and configures
// itself. The discovery endpoint only ever sees unguessable refs -> ciphertext (zero-knowledge), exactly
// like the data store. bootstrapRef is distinct from profileRef (different hash prefix), so discovery and
// data may even share one store without colliding. See docs/enrollment.md §3. ----

func bootstrapRef(name, passphrase string) string {
	h := sha256.Sum256([]byte("diaspore-bootstrap-ref:" + name + "\x00" + passphrase))
	return hex.EncodeToString(h[:])
}

// bootstrapKey wraps the sealed store-config -- a key separate from the data/pass key (its own salt).
func bootstrapKey(name, passphrase string) []byte {
	salt := sha256.Sum256([]byte("diaspore-bootstrap-salt:" + name))
	return argon2.IDKey([]byte(passphrase), salt[:], 1, 64*1024, 4, 32)
}

// publishDiscovery seals the store-config (the 5 S3_* conf lines) under bootstrapKey(name,pass) and PUTs it
// to the discovery store at bootstrapRef -- so a future device can discover it from name+pass alone.
func publishDiscovery(discoStore, name, pass, ep, rg, bk, ak, sk string) {
	cfg := fmt.Sprintf("S3_ENDPOINT=%s\nS3_REGION=%s\nS3_BUCKET=%s\nS3_ACCESS_KEY=%s\nS3_SECRET_KEY=%s\n", ep, rg, bk, ak, sk)
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
	h.Write([]byte("diaspore-head-sig"))
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

// ---- Rollback anchor (OPT-IN via DIASPORE_ROLLBACK_ANCHOR=1). Persists the highest head version seen per
// profileRef in DIASPORE_ROLLBACK_DIR (default /data/diaspore/rollback, which survives the power-off wipe)
// and rejects a head whose version is behind it -- a replay of an old, validly-signed head. OFF by default:
// the anchor records (as unguessable hashes) that profiles logged in here, trading the device's plausible
// deniability -- so it's an explicit choice for trusted / non-duress deployments. ----

func rollbackEnforced() bool { return os.Getenv("DIASPORE_ROLLBACK_ANCHOR") == "1" }

func anchorDir() string {
	if d := os.Getenv("DIASPORE_ROLLBACK_DIR"); d != "" {
		return d
	}
	return "/data/diaspore/rollback"
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
// throttles CREATE: capacity DIASPORE_ENROLL_MAX (default 5), fully refilled over DIASPORE_ENROLL_WINDOW
// seconds (default 3600). A fresh device starts full (a burst of MAX covers normal first-run setup), then
// allows ~WINDOW/MAX between creates. State lives at /data/diaspore/enroll, which -- like the rollback anchor
// and the conf -- survives the power-off wipe, so the limit can't be reset by power-cycling. This is a LOCAL
// throttle (one device); the cross-device / store-side enrollment gate is a Phase-2 broker. Set
// DIASPORE_ENROLL_MAX=0 to disable (turnkey / trusted deployments). ----

type enrollBucket struct {
	Tokens float64 `json:"tokens"`
	TS     int64   `json:"ts"`
}

var enrollMu sync.Mutex

func enrollStatePath() string {
	if p := os.Getenv("DIASPORE_ENROLL_STATE"); p != "" {
		return p
	}
	return "/data/diaspore/enroll"
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
// retryAfterSeconds) when the bucket is empty. No-op (always allows) when DIASPORE_ENROLL_MAX<=0.
func enrollAllow() (bool, int) {
	max := enrollFloat("DIASPORE_ENROLL_MAX", 5)
	if max <= 0 {
		return true, 0 // limiter disabled
	}
	win := enrollFloat("DIASPORE_ENROLL_WINDOW", 3600)
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
	head := getRef(base, profileRef(name, pass))
	if head == "" {
		return nil, nil, false
	}
	blob := getBlob(base, head)
	if v, isV := parseVault(blob); isV {
		dk := unwrapDK(v, "pass", deriveKey(name, pass))
		if dk == nil {
			return nil, nil, false // ref resolved but pass slot won't open -> treat as blank (defensive)
		}
		if v.Sig != "" && !verifyVault(v, dk) {
			fail("head signature invalid -- the store served a tampered head") // a signed head must verify
		}
		checkRollback(profileRef(name, pass), v.Version) // no-op unless rollback enforcement is enabled
		return dk, getBlob(base, v.Manifest), true
	}
	return deriveKey(name, pass), blob, true // legacy: the head blob IS the sealed manifest; DK == old key
}

// ---- Vault operations (shared by the CLI verbs and the login daemon). Panic on store/crypto error. ----

// createVault enrolls a NEW vault profile (random DK, empty data) and returns the 12-word recovery code.
func createVault(base, name, pass string) string {
	dk := randBytes(32)
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
	putRef(base, profileRef(name, pass), vh)
	putRef(base, v.RecoveryRef, vh)
	return mnemonic
}

// migrateVault upgrades a legacy (bare-manifest) profile to a vault IN PLACE -- DK = the old derived key, so
// NO data re-encryption -- and returns its new recovery code. ("", false) if it's already a vault / no ref.
func migrateVault(base, name, pass string) (string, bool) {
	head := getRef(base, profileRef(name, pass))
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
	putRef(base, profileRef(name, pass), vh)
	putRef(base, v.RecoveryRef, vh)
	return mnemonic, true
}

// rotateVault re-wraps DK under a new passphrase (no data re-encryption) and invalidates the old ref.
func rotateVault(base, name, oldpass, newpass string) error {
	head := getRef(base, profileRef(name, oldpass))
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
	newRef := profileRef(name, newpass)
	putRef(base, newRef, vh)
	if oldRef := profileRef(name, oldpass); oldRef != newRef {
		putRef(base, oldRef, "") // invalidate the old passphrase -- its ref no longer resolves (blank)
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
	signVault(nv, dk)
	vh := postBlob(base, serializeVault(nv))
	putRef(base, profileRef(name, newpass), vh)
	putRef(base, rref, vh)
	return nil
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

// ---- S3 backend (any S3-compatible store). Selected by base=="s3"; config from env. ----
var s3client *minio.Client
var s3bucket string

// workerMu serializes ALL diaspore_roamd interactions -- roam.req/roam.res are a single shared slot, so the
// periodic background seal (syncLoop) must never run concurrently with a socket-driven login/logout.
var workerMu sync.Mutex

func s3Init() {
	if s3client != nil {
		return
	}
	ep := os.Getenv("S3_ENDPOINT")
	secure := !strings.HasPrefix(ep, "http://")
	host := strings.TrimPrefix(strings.TrimPrefix(ep, "https://"), "http://")
	region := os.Getenv("S3_REGION")
	if region == "" || region == "auto" {
		region = "us-east-1"
	}
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
	obj, err := s3client.GetObject(context.Background(), s3bucket, key, minio.GetObjectOptions{})
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

func s3Put(key string, data []byte) {
	s3Init()
	_, err := s3client.PutObject(context.Background(), s3bucket, key, bytes.NewReader(data),
		int64(len(data)), minio.PutObjectOptions{ContentType: "application/octet-stream"})
	check(err)
}

func s3Exists(key string) bool {
	s3Init()
	_, err := s3client.StatObject(context.Background(), s3bucket, key, minio.StatObjectOptions{})
	return err == nil
}

func isS3(base string) bool { return base == "s3" }

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
	region := os.Getenv("DISCO_REGION")
	if region == "" || region == "auto" {
		region = "us-east-1"
	}
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
		b, ok := s3Get("ref/" + ref)
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
		b, ok := s3Get("blob/" + key)
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
		if s3Exists("blob/" + key) {
			fmt.Fprintf(os.Stderr, "  dedup blob/%s (already in store)\n", key[:12])
		} else {
			s3Put("blob/"+key, data)
		}
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
		s3Put("ref/"+ref, []byte{}) // tombstone to empty (strongly consistent; never hard-delete the key)
		return
	}
	putRef(base, ref, "")
}

// deleteProfile drops a profile's head + recovery refs so (name,pass) no longer resolves. Returns false
// (deletes nothing) if the profile doesn't resolve -- unknown name OR wrong passphrase -- so a bad pass
// can't wipe a profile. Blobs are left for store GC. Shared by the `delete-profile` CLI verb and the login
// daemon's DELETE verb (the user-facing "delete my profile").
func deleteProfile(base, name, pass string) bool {
	head := getRef(base, profileRef(name, pass))
	if head == "" {
		return false
	}
	if v, isV := parseVault(getBlob(base, head)); isV && v.RecoveryRef != "" {
		delRef(base, v.RecoveryRef) // also kill the 12-word recovery path so the name is fully gone
	}
	delRef(base, profileRef(name, pass))
	// Verify the head actually stopped resolving -- so a store that refused the delete reports failure
	// (NOTFOUND -> "couldn't delete") instead of a false "deleted" that the profile survives.
	return getRef(base, profileRef(name, pass)) == ""
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
func pushProfile(base, profile, pass, src string) {
	head := getRef(base, profileRef(profile, pass))
	if head == "" && !strings.Contains(profile, "#") {
		fmt.Printf("push: profile %q has no head -> skip (not resurrecting a deleted/absent profile)\n", profile)
		return
	}
	var dk []byte
	var v *vault
	if head != "" {
		if pv, isV := parseVault(getBlob(base, head)); isV {
			v = pv
			if dk = unwrapDK(v, "pass", deriveKey(profile, pass)); dk == nil {
				fail("push: pass slot won't unwrap")
			}
		}
	}
	if dk == nil {
		dk = deriveKey(profile, pass)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarDirTo(src, pw)) }()
	var chunks []string
	cdcSplit(pr, func(c []byte) { chunks = append(chunks, postBlob(base, seal(dk, c))) })
	mj, _ := json.Marshal(chunkManifest{Version: 1, Chunks: chunks})
	manifestHash := postBlob(base, seal(dk, append(append([]byte{}, cdcMagic...), mj...)))
	if v != nil {
		v.Manifest = manifestHash // keep the keyslots; just repoint the data + both refs
		v.Version++               // bump + re-sign so the new head is fresh + tamper-evident
		signVault(v, dk)
		vh := postBlob(base, serializeVault(v))
		putRef(base, profileRef(profile, pass), vh)
		if v.RecoveryRef != "" {
			putRef(base, v.RecoveryRef, vh)
		}
		bumpAnchor(profileRef(profile, pass), v.Version) // advance the rollback anchor (no-op unless enabled)
		fmt.Printf("push: profile %q (vault) -> %d chunks, gen %s v%d\n", profile, len(chunks), short(vh), v.Version)
	} else {
		putRef(base, profileRef(profile, pass), manifestHash)
		fmt.Printf("push: profile %q -> %d chunks, gen %s\n", profile, len(chunks), short(manifestHash))
	}
}

// tarDirTo streams a tar of dir into w -- no whole-tree buffer (the CDC push pipes this through the
// chunker so memory stays bounded to one chunk even for a multi-GB /data/media).
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
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil
		}
		hdr.Name = rel
		if !info.Mode().IsRegular() {
			return tw.WriteHeader(hdr)
		}
		// A regular file under a LIVE (running) user can grow/shrink/vanish between the stat above and
		// the read below (apps writing SQLite DBs, caches, ...). Write EXACTLY hdr.Size bytes -- cap a
		// file that grew (else `archive/tar: write too long` aborts the seal), zero-pad one that shrank,
		// skip one that vanished -- so churn can never desync the tar. A torn read of one file is fine
		// for a live snapshot (same as any running-filesystem backup; app journaling recovers on restore).
		f, err := os.Open(p)
		if err != nil {
			return nil // vanished after the walk -> skip entirely (no header written)
		}
		defer f.Close()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		n, err := io.CopyN(tw, f, hdr.Size)
		if err != nil && err != io.EOF {
			return err
		}
		if n < hdr.Size { // file shrank: pad the entry to its declared size
			_, err = io.CopyN(tw, zeroReader{}, hdr.Size-n)
			return err
		}
		return nil
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
// workers (diaspore_roamd restoring /data/user/N, diaspore_otad restoring /data/ota_package), so an entry
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
		os.MkdirAll(filepath.Dir(target), 0o700)
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
		check(err)
		_, err = io.Copy(f, tr)
		f.Close()
		check(err)
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
}

var cdcMagic = []byte("DSPRCDC1")

// ---- chunk cache: optional on-disk delta-download cache for CDC restore (DIA-20260618-02) ----
// CDC chunks are content-addressed (the manifest stores h = hex(sha256(sealed chunk))), so a chunk is
// immutable and reusable across versions: with convergent sealing an unchanged region of a v2->v3 OS
// payload (or a re-login's data) seals to identical ciphertext -> identical h. Caching the sealed blob by
// h lets a restore network-fetch ONLY the chunks it does not already have -- the P4.4b "true delta
// download" follow-on. The cache holds only CIPHERTEXT (like the store), so it is safe at rest. Enabled by
// DIASPORE_CHUNK_CACHE=<dir>; empty disables it (restore fetches every chunk, as before).
var chunkCacheDir = os.Getenv("DIASPORE_CHUNK_CACHE")
var chunkHits, chunkMiss int

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
// the cache. The CDC restore loop is sequential, so the hit/miss counters need no locking.
func getChunk(base, h string) []byte {
	if b, ok := cacheRead(chunkCacheDir, h); ok {
		chunkHits++
		return b
	}
	b := getBlob(base, h)
	chunkMiss++
	cacheWrite(chunkCacheDir, h, b)
	return b
}

// ---- OTA version marker (DIA-20260618-03 self-service OTA) ----
// The latest published OS version lives in a small plain store ref (the OS is public; the version is not
// secret). `ota-mark` sets it on publish; `ota-check` compares it to the running /system stamp so the device
// knows whether a newer OS exists WITHOUT downloading the (large) payload first.
const otaVersionRef = "ota/fp3/version"

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
		runFile = "/system/etc/diaspore-ota-version"
	}
	running := "0.0.0"
	if b, err := os.ReadFile(runFile); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			running = s
		}
	}
	latest = getRef(base, otaVersionRef)
	return latest, latest != "" && semverLess(running, latest)
}

// restoreSet restores a profile's working set (items with prio <= maxPrio) into dest, returning the
// number of items restored. Shared by the `restore-set` CLI command and the login daemon. On any
// failure (wrong passphrase, unknown profile, missing blob) the helpers panic; callers recover and
// treat that as a blank result (blind login).
func restoreSet(base, profile, pass, dest string, maxPrio int, verbose bool) (int, bool) {
	key := deriveKey(profile, pass)
	head := getRef(base, profileRef(profile, pass))
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
	putRef(base, profileRef(profile, pass), postBlob(base, seal(key, mj)))
	return len(m.Items)
}

// loginDaemon is the root side of the blind-login chooser. init creates an AF_UNIX socket
// (`socket diaspore_login` in diaspore.rc) and passes its fd via env ANDROID_SOCKET_diaspore_login.
// The chooser app connects, sends "<name>\n<pass>\n", and we restore that profile's working set into
// DIASPORE_STATE (default /data/diaspore/state, the tmpfs). The passphrase only ever lives in this
// process's memory -- never on disk, never in any argv. Reply: "OK <items>\n" or "BLANK\n" (uniform
// blank on wrong pass / unknown profile -> blind login / plausible deniability).
// pinnedChooserApp is the appId (uid modulo the 100000 per-user offset) of the first socket peer the daemon
// sees -- trust-on-first-use. The device boots straight into the kiosk gate (the only app at boot; no roamed
// user exists yet), so the first peer IS the chooser; we pin it and reject any later peer with a different
// appId. Defense-in-depth (audit #2): SELinux's connectto restriction is the primary gate, this is a belt
// for an ever-permissive build, and it needs no privileged read (SO_PEERCRED is just getsockopt). -1 = unpinned.
var pinnedChooserApp = -1

func loginDaemon() {
	dest := os.Getenv("DIASPORE_STATE")
	if dest == "" {
		dest = "/data/diaspore/state"
	}
	fd, err := strconv.Atoi(os.Getenv("ANDROID_SOCKET_diaspore_login"))
	if err != nil {
		fail("login-daemon: no ANDROID_SOCKET_diaspore_login fd from init")
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
		// Defense-in-depth (DIA-20260618-08, audit #2): SELinux is the primary gate (only the diaspore_chooser
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
		f := os.NewFile(uintptr(nfd), "diaspore_login_conn")
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
		// "SET-STORE\n<endpoint>\n<region>\n<bucket>\n<accesskey>\n<secretkey>\n" -> write diaspore.conf + reload.
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
	if name == "CREATE" {
		// "CREATE\n<name>\n<pass>\n" -> enroll a brand-new identity (Day-0).
		cname, _ := r.ReadString('\n')
		cname = strings.TrimRight(cname, "\r\n")
		cpass, _ := r.ReadString('\n')
		cpass = strings.TrimRight(cpass, "\r\n")
		handleCreate(conn, dest, cname, cpass)
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
		handleRoamIn(conn, dest, strings.TrimRight(rname, "\r\n"), strings.TrimRight(rpass, "\r\n"), strings.TrimRight(ruid, "\r\n"))
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
	if name == "DELETE" {
		// "DELETE\n<name>\n<pass>\n" -> user-facing "delete my profile": drop it from the store (the typed
		// pass re-authenticates), then wipe the local session WITHOUT sealing.
		dname, _ := r.ReadString('\n')
		dpass, _ := r.ReadString('\n')
		handleDelete(conn, dest, strings.TrimRight(dname, "\r\n"), strings.TrimRight(dpass, "\r\n"))
		return
	}
	if name == "POLL-REAP" {
		// The user-0 chooser polls this; hand back the next no-reboot-logoff step (SWITCH then REAP, once each)
		// so it drives the teardown from its device-owner context (the worker can't -- device-owner wall).
		fmt.Fprintf(conn, "%s\n", nextReapAction())
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
				markRoamSession(dest, rname, rnewp, su)
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
// level: lives in /data/diaspore (alongside the store conf), so it survives reboots + the power-off
// user-data wipe and is cleared only by a factory reset; default off. Not user data, so it never roams.
const otaAutoPath = "/data/diaspore/ota-auto"

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

// handleOtaApply triggers the su:s0 updater on a user-confirmed install: write the request into the RAM
// tmpfs + `ctl.start diaspore_otad` (the confined daemon's only way to spawn an su:s0 service; mirrors the
// roamd trigger). The updater re-checks, downloads (delta via the chunk cache), update_engine-applies, and
// reboots into the new slot -- so there is no result to wait for; the daemon just acks.
func handleOtaApply(conn net.Conn, dest string) {
	os.WriteFile(filepath.Join(dest, "ota.req"), []byte("apply\n"), 0o600)
	exec.Command("/system/bin/setprop", "ctl.start", "diaspore_otad").Run()
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
	if getRef("s3", profileRef(name, pass)) != "" {
		fmt.Fprint(conn, "EXISTS\n") // this exact (name, pass) is already enrolled
		return
	}
	mnemonic := ""
	ok := false
	func() {
		defer func() { recover() }()
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
	} else {
		fmt.Fprint(conn, "BLANK\n")
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

// triggerRoamWorker hands the su:s0 data worker (diaspore_roamd) a request via the RAM tmpfs and kicks it
// with ctl.start -- the only way this confined daemon can spawn an su:s0 service -- then waits for the
// result. The worker does the privileged /data/user/N restore/seal + restorecon this domain can't.
// Returns the worker's result line ("OK <uid>" | "BLANK" | "ERR-...") or "ERR-TIMEOUT".
func triggerRoamWorker(dest, op, name, pass, uid string, onProg func(string)) string {
	workerMu.Lock() // one worker run at a time (login / logout / periodic seal share roam.req/res)
	defer workerMu.Unlock()
	req := filepath.Join(dest, "roam.req")
	res := filepath.Join(dest, "roam.res")
	prog := filepath.Join(dest, "roam.progress")
	os.Remove(res)
	os.Remove(prog) // clear any stale progress from a previous roam
	if err := os.WriteFile(req, []byte(op+"\n"+name+"\n"+pass+"\n"+uid+"\n"), 0o600); err != nil {
		return "ERR-REQ"
	}
	exec.Command("/system/bin/setprop", "ctl.start", "diaspore_roamd").Run()
	last := ""
	for i := 0; i < 120; i++ { // restore/seal does S3 I/O -> wait up to ~60s
		time.Sleep(500 * time.Millisecond)
		if onProg != nil { // relay the worker's chunk progress to the caller (the gate's restore bar)
			if b, err := os.ReadFile(prog); err == nil {
				if s := strings.TrimRight(string(b), "\r\n"); s != "" && s != last {
					last = s
					onProg(s)
				}
			}
		}
		if b, err := os.ReadFile(res); err == nil {
			if out := strings.TrimRight(string(b), "\r\n"); out != "" {
				os.Remove(res)
				os.Remove(prog)
				return out
			}
		}
	}
	return "ERR-TIMEOUT"
}

// handleRoamIn drives the worker to restore profile (name,pass) into the chooser-created user <uid>, and
// -- ONLY on success -- records the roam session so logout re-seals the SAME user. A bad cred / missing
// profile yields an empty user (BLANK, indistinguishable -- blind login) and is NOT recorded, so logout
// can never seal an empty user over a real profile.
func handleRoamIn(conn net.Conn, dest, name, pass, uid string) {
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
	conn.SetDeadline(time.Now().Add(150 * time.Second)) // restore can take ~60s; streamed progress + result
	// Stream the worker's restore progress to the gate as "PROGRESS <phase> <done> <total>" lines; the
	// chooser drives a determinate bar off them, then reads the final OK/BLANK line.
	out := triggerRoamWorker(dest, "in", name, pass, uid, func(p string) {
		fmt.Fprintf(conn, "PROGRESS %s\n", p)
	})
	if strings.HasPrefix(out, "OK") {
		// Auto-upgrade a legacy profile to the keyslot vault on first login (no re-encryption). BEFORE
		// markRoamSession so the continuous sync (which keys off .roamsession) can't race the migration.
		recovery := ""
		func() {
			defer func() { recover() }()
			if m, migrated := migrateVault("s3", name, pass); migrated {
				recovery = m
			}
		}()
		markRoamSession(dest, name, pass, uid)
		autoPublish(name, pass) // keep discovery in sync: this identity -> the current store config
		fmt.Fprintf(os.Stderr, "[logind] roam-in %q -> user %s (migrated=%v)\n", name, uid, recovery != "")
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
	out := triggerRoamWorker(dest, "out", name, pass, uid, nil) // logoff: no progress UI (we reboot after)
	os.Remove(filepath.Join(dest, ".roamsession"))
	fmt.Fprintf(os.Stderr, "[logind] roam-out %q user %s (%s)\n", name, uid, out)
	fmt.Fprint(conn, "OK\n")
	// Data is sealed -> reboot to WIPE the ephemeral roamed user and bring the gate back up. The chooser
	// can't reboot from a secondary user (PowerManager.reboot is blocked there); this root daemon can, via
	// sys.powerctl. Brief pause so the OK reply reaches the chooser before init tears us down.
	time.Sleep(500 * time.Millisecond)
	exec.Command("/system/bin/setprop", "sys.powerctl", "reboot").Run()
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
var pendingReapUID string   // phase 2: chooser removes the user (after the seal)

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
	return "NONE"
}

func handleLogout(conn net.Conn, dest string) {
	name, pass, uid := readRoamSession(dest)
	if name == "" || uid == "" {
		fmt.Fprint(conn, "OK\n") // no live roam session -> nothing to do
		return
	}
	// Two-phase reap (see nextReapAction). Remove .roamsession FIRST so the periodic sync can't reseal a dead
	// user, then queue the SWITCH the chooser does almost immediately -- the gate is back in ~1.5s, leaving
	// essentially no window to gesture out of "Logging off…". Reply at once; the seal runs in the background.
	os.Remove(filepath.Join(dest, ".roamsession"))
	pendingReapMu.Lock()
	pendingSwitchUID = uid
	pendingReapMu.Unlock()
	fmt.Fprintf(os.Stderr, "[logind] logout(no-reboot) %q user %s: switch queued, sealing in background\n", name, uid)
	fmt.Fprint(conn, "OK\n")
	// Seal the backgrounded-but-still-RUNNING user (CE still unlocked), THEN queue the remove. Proceed
	// regardless of the seal result -- the continuous sync holds a recent copy, so logout can't strand data.
	go func() {
		out := triggerRoamWorker(dest, "out", name, pass, uid, nil)
		pendingReapMu.Lock()
		pendingReapUID = uid
		pendingReapMu.Unlock()
		fmt.Fprintf(os.Stderr, "[logind] logout seal %q user %s (%s); reap queued\n", name, uid, out)
	}()
	// Fallback: if no chooser consumes the SWITCH within 8s (its process is dead), reboot to wipe+gate.
	go func() {
		time.Sleep(8 * time.Second)
		pendingReapMu.Lock()
		stuck := pendingSwitchUID == uid
		if stuck {
			pendingSwitchUID = ""
			pendingReapUID = ""
		}
		pendingReapMu.Unlock()
		if stuck {
			fmt.Fprintf(os.Stderr, "[logind] switch not consumed in 8s (no watcher) -> reboot fallback\n")
			exec.Command("/system/bin/setprop", "sys.powerctl", "reboot").Run()
		}
	}()
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
	if loginName != "" && getRef(base, profileRef(loginName, loginPass)) != "" {
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
	out := triggerRoamWorker(dest, "settz", zone, "", "", nil) // zone in the name slot; pass/uid unused
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
// without us having to spoof a browser UA (which would be brittle + against ToS) or leak a "Diaspore" UA.
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
// ships with no store). The conf lives at /data/diaspore/diaspore.conf -- device-level, persists the
// power-off wipe (only the per-user roamed data is amnesiac) and is rotatable without a reflash. ---

const storeConfPath = "/data/diaspore/diaspore.conf"

// storeConfigured reports whether the daemon has a usable S3 store config (endpoint + bucket + creds).
func storeConfigured() bool {
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

// handleSetStore writes the device store config to diaspore.conf (root-only, LF -- a CR breaks the agent's
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

// applyStoreConf writes the data-store config to diaspore.conf AND applies it to the running daemon (env +
// reset the s3 client) so it takes effect with no reboot. Shared by SET-STORE and discovery.
func applyStoreConf(ep, rg, bk, ak, sk string) error {
	if rg == "" {
		rg = "us-east-1"
	}
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
		os.Getenv("S3_ACCESS_KEY"), os.Getenv("S3_SECRET_KEY"))
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
	fmt.Fprintf(os.Stderr, "[logind] discovered + applied store config for %q\n", name)
	return true
}

// writeStoreConf writes the S3_* lines (LF) and PRESERVES any non-S3 lines (e.g. DIASPORE_SYNC_INTERVAL,
// DIASPORE_DNS) from an existing conf, at 0600 root.
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
	os.MkdirAll("/data/diaspore", 0o700)
	return os.WriteFile(storeConfPath, []byte(sb.String()), 0o600)
}

// handleTestStore validates a store config WITHOUT saving it -- the Settings screen's "Test connection".
// Builds a throwaway minio client with the given creds (DNS resolves via the global DIASPORE_DNS resolver,
// like every other op) and does a lightweight BucketExists, mapping the outcome to a UI-friendly verdict.
func handleTestStore(conn net.Conn, ep, rg, bk, ak, sk string) {
	if ep == "" || bk == "" || ak == "" || sk == "" {
		fmt.Fprint(conn, "ERR-FIELDS\n")
		return
	}
	if rg == "" || rg == "auto" {
		rg = "us-east-1"
	}
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

// markRoamSession / readRoamSession persist (name, pass, uid) for the live roamed user in the RAM tmpfs
// -- same trick as .session: a plain file, so the sync's push-set skips it and it's gone on power-off.
func markRoamSession(dest, name, pass, uid string) {
	os.WriteFile(filepath.Join(dest, ".roamsession"), []byte(name+"\n"+pass+"\n"+uid+"\n"), 0o600)
}

// syncLoop continuously seals the LIVE roamed user's data to the store so an unclean power-off (battery,
// crash) loses at most one interval instead of the whole session -- the store is the only durable copy
// (/data is wiped on power-off). It reuses the worker's "out" op (push /data/user/N + user_de + media)
// WITHOUT the logoff reboot, serialized with login/logout via workerMu (inside triggerRoamWorker).
// Convergent sealing dedups unchanged data, so an idle cycle uploads nothing. No live session => skip.
// Supersedes the Arc-1 diaspore_sync.sh, which keyed off the tmpfs .session marker + pushed the
// working-set dir -- both wrong for the Arc-2 per-user model (it skipped every cycle).
func syncLoop(dest string) {
	interval := 120 * time.Second
	if s := os.Getenv("DIASPORE_SYNC_INTERVAL"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}
	for {
		time.Sleep(interval)
		name, pass, uid := readRoamSession(dest)
		if name == "" || uid == "" {
			continue // nobody logged in -> nothing to seal
		}
		out := triggerRoamWorker(dest, "out", name, pass, uid, nil) // seal only -- no reboot, no marker clear
		fmt.Fprintf(os.Stderr, "[logind] periodic seal %q user %s -> %s\n", name, uid, out)
	}
}

func readRoamSession(dest string) (name, pass, uid string) {
	b, err := os.ReadFile(filepath.Join(dest, ".roamsession"))
	if err != nil {
		return "", "", ""
	}
	p := strings.SplitN(strings.TrimRight(string(b), "\r\n"), "\n", 3)
	for len(p) < 3 {
		p = append(p, "")
	}
	return p[0], p[1], p[2]
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "ERR:", r)
			os.Exit(1)
		}
	}()
	// Android provides no /etc/resolv.conf and empty net.dns*, so Go's pure resolver falls back to
	// localhost:53 and fails. When DIASPORE_DNS (host:port, e.g. "8.8.8.8:53") is set, route all DNS
	// through it. Opt-in, so host/VM behaviour (system resolver) is unchanged. (Production TODO: use
	// the device's real per-network DNS via netd instead of a fixed public resolver — privacy.)
	if dns := os.Getenv("DIASPORE_DNS"); dns != "" {
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", dns)
			},
		}
	}
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
		putRef(os.Args[2], otaVersionRef, os.Args[3])
		fmt.Printf("ota-mark: %s = %s\n", otaVersionRef, os.Args[3])

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
			// Progress: report chunks fetched (the slow, network-bound step) to DIASPORE_PROGRESS so the
			// login daemon can stream "<phase> <done> <total>" to the gate's restore bar. DIASPORE_PHASE
			// (apps|secure|media) is set per call by the roam worker; no-op when the env isn't set (CLI/host).
			total := len(m.Chunks)
			writeProg(0, total)
			// Stream the chunks (in order) through a pipe into the untar -> bounded memory.
			pr, pw := io.Pipe()
			go func() {
				var e error
				for i, h := range m.Chunks {
					if _, e = pw.Write(unseal(key, getChunk(base, h))); e != nil {
						break
					}
					writeProg(i+1, total)
				}
				pw.CloseWithError(e)
			}()
			untarFrom(dest, pr)
			if chunkCacheDir != "" {
				fmt.Printf("restore: profile %q (%d chunks: %d cached, %d fetched) -> %s\n", profile, len(m.Chunks), chunkHits, chunkMiss, dest)
			} else {
				fmt.Printf("restore: profile %q (%d chunks) -> %s\n", profile, len(m.Chunks), dest)
			}
		} else {
			untarFrom(dest, bytes.NewReader(pt)) // pre-CDC ref: the blob IS the tar
			fmt.Printf("restore: profile %q (legacy %d B) -> %s\n", profile, len(pt), dest)
		}

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
		oh := sha256.Sum256([]byte("diaspore-ref:" + name)) // the OLD name-only scheme
		head := getRef(base, hex.EncodeToString(oh[:]))
		if head == "" {
			fmt.Printf("migrate-ref: no old ref for %q\n", name)
			return
		}
		putRef(base, profileRef(name, pass), head) // re-point only; content-addressed blobs are unchanged
		fmt.Printf("migrate-ref: %q -> name+pass ref now points to %s\n", name, short(head))

	case "create-vault": // create-vault <store> <name> <pass>   (new profile in the vault model; prints the 12-word code)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		if getRef(base, profileRef(name, pass)) != "" {
			fail("create-vault: profile already exists")
		}
		fmt.Printf("RECOVERY %s\n", createVault(base, name, pass))

	case "migrate-vault": // migrate-vault <store> <name> <pass>   (upgrade a legacy profile to a vault; prints the code)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		if getRef(base, profileRef(name, pass)) == "" {
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

	case "delete-profile": // delete-profile <store> <name> <pass>   (drop the head + recovery refs; blobs are left for store GC)
		base, name, pass := os.Args[2], os.Args[3], os.Args[4]
		if deleteProfile(base, name, pass) {
			fmt.Printf("delete-profile: %q removed\n", name)
		} else {
			fmt.Printf("delete-profile: %q not found / wrong passphrase\n", name)
		}

	case "publish-discovery": // publish-discovery <discoStore> <name> <pass> <endpoint> <region> <bucket> <key> <secret>
		ds, n, p := os.Args[2], os.Args[3], os.Args[4]
		publishDiscovery(ds, n, p, os.Args[5], os.Args[6], os.Args[7], os.Args[8], os.Args[9])
		fmt.Printf("publish-discovery: %q published (ref %s…)\n", n, short(bootstrapRef(n, p)))

	case "discover": // discover <discoStore> <name> <pass>   (fetch + unseal this identity's store-config)
		ds, n, p := os.Args[2], os.Args[3], os.Args[4]
		if cfg, ok := discoverConfig(ds, n, p); ok {
			fmt.Print(cfg) // the S3_* conf lines
		} else {
			fmt.Println("NOTFOUND")
		}

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

// writeProg reports restore progress to the file named by DIASPORE_PROGRESS as one line
// "<phase> <done> <total>", atomically (temp+rename) so a concurrent reader -- the login daemon, which
// streams it to the gate's restore bar -- never sees a torn write. DIASPORE_PHASE labels which class of
// data this restore is (apps|secure|media). No-op when DIASPORE_PROGRESS isn't set (CLI / host use).
func writeProg(done, total int) {
	file := os.Getenv("DIASPORE_PROGRESS")
	if file == "" {
		return
	}
	phase := os.Getenv("DIASPORE_PHASE")
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
	fmt.Println("usage: diaspore_agent <get|restore|push|push-set|restore-set|create-vault|migrate-vault|rotate|recover|delete-profile|ota-mark|ota-check|publish-discovery|discover|login-daemon> ...")
	os.Exit(2)
}
