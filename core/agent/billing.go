package main

// Billing client -- the device half of the managed-store gateway (Nowhere Phase 2). The gateway sells
// capacity as capability tokens and keeps data alive with per-ref leases; here the device holds the token
// wallet (which roams as ordinary encrypted state), computes owed rent from a profile's footprint, and
// redeems. Tokens are OPAQUE to the device: it stores and presents them; the gateway signs/verifies. The
// gateway URL comes from GATEWAY_URL (set from the store config, like the S3_* vars). Step 1 mints via a
// dev faucet -- real payment rails are a later slice. See the private nowhere-phone build plan.

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"nowhereagent/blindrsa"
)

const giB = 1 << 30

// billingEpochSeconds is the lease epoch length (1 week) -- the cadence rent renews at. The gateway
// reports this in its quote; we mirror it as a constant so the Profile's Storage view can render
// "paid through <date>" + "renews weekly" with no gateway round-trip.
const billingEpochSeconds = 604800

// Token mirrors the gateway's issued token on the wire; the device treats it as opaque.
type Token struct {
	Denom string `json:"denom"`
	Nonce string `json:"nonce"`
	Exp   int64  `json:"exp"`
	Sig   string `json:"sig"`
}

// BlindToken is a zero-knowledge storage token (the device half of the blind-issuance arc, P3b): the
// signed message "denom|nonce|exp" plus the unblinded RSA-PSS blind signature, both base64 (std). It is
// what the gateway issues unlinkably (POST /issue) and verifies at redeem (the blind_tokens field of
// /lease). Unlike a faucet Token, the gateway never saw this token at issuance, so it can't tie the
// redemption to the payment. The device treats it as opaque the same way -- it only stores + presents it.
type BlindToken struct {
	Msg string `json:"msg"`
	Sig string `json:"sig"`
}

// Wallet is the set of unspent tokens plus the epoch its lease is paid through. It roams as ordinary
// encrypted state -- sealed/restored under the profile's data key (see sealWallet/restoreWallet) -- so it
// survives the power-off wipe with no special on-device storage. PaidThrough lets payRent be idempotent
// within a lease window: called on every sync, it only redeems once the current epoch is no longer covered.
// BlindTokens are zero-knowledge credit (P3b); plain Tokens are dev-faucet credit. Both spend at /lease.
type Wallet struct {
	Tokens      []Token      `json:"tokens"`
	BlindTokens []BlindToken `json:"blind_tokens,omitempty"`
	PaidThrough int64        `json:"paid_through"` // last epoch the profile's lease is paid through (0 = never leased)
	// Subscription (P3): the bearer subscription secret + the last billing epoch we refilled, so an
	// auto-refill runs once per epoch. Vouchers holds any pulled-but-not-yet-redeemed vouchers, persisted
	// before /refill so an interrupted refill resumes without losing paid budget. All roam with the wallet.
	SubKey   string    `json:"sub_key,omitempty"`
	SubEpoch int64     `json:"sub_epoch,omitempty"`
	Vouchers []voucher `json:"vouchers,omitempty"`
}

func loadWallet(path string) (*Wallet, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Wallet{}, nil
		}
		return nil, err
	}
	var w Wallet
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func saveWallet(path string, w *Wallet) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// balance is total spendable credit: zero-knowledge (blind) tokens plus dev-faucet tokens.
func (w *Wallet) balance() int { return len(w.Tokens) + len(w.BlindTokens) }

// billingLine renders the Profile "Storage & subscription" view from the LOCAL wallet only -- no
// passphrase, no gateway round-trip, so it can't fail or hang on a tap. `credit` is GB-months of
// unspent capacity; `through` is the epoch the lease is paid through (0 = never leased); `epochsec`
// lets the UI turn `through` into a renewal date. "NONE" when there's no wallet / nothing leased
// yet (a throwaway, or a store profile that hasn't paid rent). GB-USED (the footprint) is a later
// increment -- it needs the store round-trip + the session's keys.
func billingLine(walletPath string) string {
	w, err := loadWallet(walletPath)
	if err != nil || (w.balance() == 0 && w.PaidThrough == 0) {
		return "NONE\n"
	}
	return fmt.Sprintf("OK credit=%d through=%d epochsec=%d\n", w.balance(), w.PaidThrough, billingEpochSeconds)
}

// take removes and returns the first n tokens; ok=false (and no mutation) if fewer than n are held.
func (w *Wallet) take(n int) ([]Token, bool) {
	if len(w.Tokens) < n {
		return nil, false
	}
	out := append([]Token(nil), w.Tokens[:n]...)
	w.Tokens = append([]Token(nil), w.Tokens[n:]...)
	return out, true
}

// takeBlind removes and returns the first n zero-knowledge tokens; ok=false (no mutation) if fewer held.
func (w *Wallet) takeBlind(n int) ([]BlindToken, bool) {
	if len(w.BlindTokens) < n {
		return nil, false
	}
	out := append([]BlindToken(nil), w.BlindTokens[:n]...)
	w.BlindTokens = append([]BlindToken(nil), w.BlindTokens[n:]...)
	return out, true
}

// choosePayment removes up to `owed` tokens from the wallet to present at a lease, PREFERRING
// zero-knowledge (blind) tokens and topping up from plain faucet tokens only if blind credit runs short.
// ok=false (wallet partially drained but unsaved by the caller) if total credit < owed. The returned
// slices are presented as the lease's plain `tokens` and `blind_tokens`; settlePayment reconciles the
// wallet afterward against how many the gateway actually charged.
func (w *Wallet) choosePayment(owed int) (plain []Token, blind []BlindToken, ok bool) {
	nBlind := owed
	if nBlind > len(w.BlindTokens) {
		nBlind = len(w.BlindTokens)
	}
	blind, _ = w.takeBlind(nBlind)
	plain, ok = w.take(owed - nBlind)
	return plain, blind, ok
}

// settlePayment returns the tokens the gateway did NOT charge (a delta lease may spend fewer than were
// presented) to the wallet. The gateway collects nonces plain-first, then blind, so the first `spent` of
// (plain ++ blind) are gone; the rest go back. Mutates the wallet in place.
func (w *Wallet) settlePayment(plain []Token, blind []BlindToken, spent int) {
	if spent < 0 {
		spent = 0
	}
	plainSpent := spent
	if plainSpent > len(plain) {
		plainSpent = len(plain)
	}
	blindSpent := spent - plainSpent
	if blindSpent > len(blind) {
		blindSpent = len(blind)
	}
	w.Tokens = append(w.Tokens, plain[plainSpent:]...)
	w.BlindTokens = append(w.BlindTokens, blind[blindSpent:]...)
}

// ---- gateway HTTP client ----

type gatewayClient struct {
	base string
	hc   *http.Client
}

func newGatewayClient(base string) *gatewayClient {
	return &gatewayClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 30 * time.Second}}
}

type quoteInfo struct {
	Denom          string `json:"denom"`
	GBPerToken     int64  `json:"gb_per_token"`
	EpochSeconds   int64  `json:"epoch_seconds"`
	EpochsPerLease int64  `json:"epochs_per_lease"`
	PriceCents     int64  `json:"price_cents_per_gb_month"`
	CurrentEpoch   int64  `json:"current_epoch"`
	IssuerPubKey   string `json:"issuer_pubkey"`
	FreeGB         int64  `json:"free_gb"` // free-tier quota per profile (GB); 0 = free path off
}

func (g *gatewayClient) getJSON(path string, out any) error {
	resp, err := g.hc.Get(g.base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *gatewayClient) postJSON(path string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	resp, err := g.hc.Post(g.base+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (g *gatewayClient) quote() (quoteInfo, error) {
	var q quoteInfo
	err := g.getJSON("/quote", &q)
	return q, err
}

type tokensReq struct {
	Count   int    `json:"count"`
	Payment string `json:"payment"`
}

type tokensResp struct {
	Tokens []Token `json:"tokens"`
}

// buyFaucet mints tokens via the dev faucet (step 1; real payment rails are a later slice).
func (g *gatewayClient) buyFaucet(count int) ([]Token, error) {
	var out tokensResp
	code, err := g.postJSON("/tokens", tokensReq{Count: count, Payment: "dev-faucet"}, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("tokens: HTTP %d", code)
	}
	return out.Tokens, nil
}

// ---- zero-knowledge (blind) token issuance (P3b): the device half of the unlinkable-billing arc ----

// blindTokenTTL is how long an issued zero-knowledge token stays redeemable. The device chooses it (it
// rides in the signed "denom|nonce|exp" message); the gateway rejects an expired token at /lease. Matches
// the gateway's faucet TokenTTL so credit doesn't quietly age out before it's spent.
const blindTokenTTL = 365 * 24 * time.Hour

type issuePubResp struct {
	PubKeyPEM string `json:"pubkey_pem"`
	Denom     string `json:"denom"`
}

// issuePubKey fetches the gateway's RSA blind-issuance public key (PKIX PEM) + the token denomination.
// The device blinds against this key and verifies the finalized signatures with it.
func (g *gatewayClient) issuePubKey() (*rsa.PublicKey, string, error) {
	var r issuePubResp
	if err := g.getJSON("/issue/pubkey", &r); err != nil {
		return nil, "", err
	}
	blk, _ := pem.Decode([]byte(r.PubKeyPEM))
	if blk == nil {
		return nil, "", errors.New("issue/pubkey: not a PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, "", err
	}
	rp, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, "", errors.New("issue/pubkey: not an RSA public key")
	}
	return rp, r.Denom, nil
}

type issueReq struct {
	Claim   string   `json:"claim"`
	Blinded []string `json:"blinded"` // base64 (std) blinded messages
}
type issueResp struct {
	BlindSigs []string `json:"blind_sigs"` // base64 (std), 1:1 with blinded
}

func (g *gatewayClient) issue(claim string, blinded []string) ([]string, int, error) {
	var out issueResp
	code, err := g.postJSON("/issue", issueReq{Claim: claim, Blinded: blinded}, &out)
	return out.BlindSigs, code, err
}

// issueBatch claims exactly n blind tokens for `claim` against the given issuer key + denom: it blinds n
// fresh denom|nonce|exp messages, POSTs /issue, then Finalizes (unblinds + verifies) each signature. It
// returns the tokens and the HTTP status, so a caller can tell a spent/insufficient claim (402) apart from
// a transport/crypto error. It does NOT touch the wallet -- the caller persists -- so a batch is atomic.
func (g *gatewayClient) issueBatch(pub *rsa.PublicKey, denom, claim string, n int) ([]BlindToken, int, error) {
	exp := time.Now().Add(blindTokenTTL).Unix()
	msgs := make([][]byte, n)
	invs := make([][]byte, n)
	blinded := make([]string, n)
	for i := 0; i < n; i++ {
		var nb [12]byte
		if _, err := rand.Read(nb[:]); err != nil {
			return nil, 0, err
		}
		// message = denom|nonce|exp -- the exact format the gateway parses at /lease (matches issuer.canonical)
		msg := []byte(denom + "|" + base64.RawURLEncoding.EncodeToString(nb[:]) + "|" + strconv.FormatInt(exp, 10))
		bl, inv, err := blindrsa.Blind(pub, msg)
		if err != nil {
			return nil, 0, err
		}
		msgs[i], invs[i], blinded[i] = msg, inv, base64.StdEncoding.EncodeToString(bl)
	}
	sigs, code, err := g.issue(claim, blinded)
	if err != nil {
		return nil, code, err
	}
	if code != http.StatusOK {
		return nil, code, nil // e.g. 402 (claim spent/insufficient) -- not an error here; the caller decides
	}
	if len(sigs) != n {
		return nil, code, fmt.Errorf("issue: got %d signatures, want %d", len(sigs), n)
	}
	toks := make([]BlindToken, n)
	for i, s := range sigs {
		bs, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, code, fmt.Errorf("issue: bad blind signature encoding: %w", err)
		}
		sig, err := blindrsa.Finalize(pub, msgs[i], bs, invs[i]) // unblinds AND verifies; fails closed
		if err != nil {
			return nil, code, fmt.Errorf("issue: finalize/verify failed: %w", err)
		}
		toks[i] = BlindToken{Msg: base64.StdEncoding.EncodeToString(msgs[i]), Sig: base64.StdEncoding.EncodeToString(sig)}
	}
	return toks, code, nil
}

// appendTokens loads the wallet, appends new zero-knowledge tokens, and saves it (atomic temp+rename).
func appendTokens(walletPath string, toks []BlindToken) error {
	w, err := loadWallet(walletPath)
	if err != nil {
		return err
	}
	w.BlindTokens = append(w.BlindTokens, toks...)
	return saveWallet(walletPath, w)
}

// claimTokens redeems a paid CLAIM code for EXACTLY `count` zero-knowledge storage tokens and adds them to
// the wallet. Unlinkability comes from blinding: the device generates each token's nonce + a blinding
// factor locally, the gateway blind-signs WITHOUT ever seeing the token, and the device unblinds. Tokens
// persist only on full success -- a partial/HTTP failure leaves the wallet untouched. (drainClaim is the
// device path used when the purchased count isn't known.)
func claimTokens(g *gatewayClient, walletPath, claim string, count int) (int, error) {
	if claim == "" {
		return 0, errors.New("claim: empty claim code")
	}
	if count <= 0 {
		return 0, errors.New("claim: count must be > 0")
	}
	pub, denom, err := g.issuePubKey()
	if err != nil {
		return 0, err
	}
	toks, code, err := g.issueBatch(pub, denom, claim, count)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("issue: HTTP %d (claim invalid, spent, or insufficient)", code)
	}
	if err := appendTokens(walletPath, toks); err != nil {
		return 0, err
	}
	return count, nil
}

// drainClaim redeems an ENTIRE paid claim code into the wallet WITHOUT knowing how many tokens it's worth
// (the device never sees the purchase -- this is the "Add credits" path). It claims in batches that double
// on success and back off on a 402 ("claim spent/insufficient"), converging on the exact remaining count
// in O(log n) round-trips and never over-claiming (the gateway reserves a batch all-or-nothing, so a too-
// large request signs nothing). Each successful batch is persisted as it lands, so an interrupted drain
// can simply be re-run on the same code to finish. Returns the tokens added (0 = unknown or fully spent).
func drainClaim(g *gatewayClient, walletPath, claim string) (int, error) {
	if claim == "" {
		return 0, errors.New("claim: empty claim code")
	}
	pub, denom, err := g.issuePubKey()
	if err != nil {
		return 0, err
	}
	const maxBatch = 256
	total, batch := 0, 1
	for {
		toks, code, err := g.issueBatch(pub, denom, claim, batch)
		switch {
		case err != nil:
			if total > 0 {
				return total, nil // keep what already landed; report partial success rather than erroring
			}
			return 0, err
		case code == http.StatusOK:
			if e := appendTokens(walletPath, toks); e != nil {
				return total, e
			}
			total += len(toks)
			if batch < maxBatch {
				batch *= 2
			}
		case code == http.StatusPaymentRequired:
			if batch == 1 {
				return total, nil // nothing left to claim -> done
			}
			batch /= 2 // overshot the remaining balance; back off and drain the tail
		default:
			if total > 0 {
				return total, nil
			}
			return 0, fmt.Errorf("issue: HTTP %d", code)
		}
	}
}

type leaseReq struct {
	Tokens      []Token          `json:"tokens"`
	BlindTokens []BlindToken     `json:"blind_tokens,omitempty"` // zero-knowledge tokens (P3b): {msg,sig} base64
	Refs        map[string]int64 `json:"refs"`
	Free        bool             `json:"free,omitempty"`    // free-tier lease (no tokens; gateway meters per profile)
	Profile     string           `json:"profile,omitempty"` // the pseudonymous profileRef the free quota is metered under
}

type leaseInfo struct {
	ThroughEpoch int64             `json:"through_epoch"`
	Spent        int               `json:"spent"`
	WriteCaps    map[string]string `json:"write_caps"`
}

func (g *gatewayClient) lease(tokens []Token, blind []BlindToken, refs map[string]int64) (leaseInfo, int, error) {
	var out leaseInfo
	code, err := g.postJSON("/lease", leaseReq{Tokens: tokens, BlindTokens: blind, Refs: refs}, &out)
	return out, code, err
}

// leaseFree requests token-less leases on the free tier (up to the gateway's per-profile quota), metered
// under the pseudonymous profileRef. It returns the lease info AND the HTTP status so the caller can tell a
// granted free lease (200) from an over-quota refusal (402 -> fall back to paid) or a disabled free tier
// (403). No wallet is touched -- the free path spends nothing.
func (g *gatewayClient) leaseFree(profile string, refs map[string]int64) (leaseInfo, int, error) {
	var out leaseInfo
	code, err := g.postJSON("/lease", leaseReq{Free: true, Profile: profile, Refs: refs}, &out)
	return out, code, err
}

// capResp is the gateway's reply to GET /cap: a single presigned URL for one ref + op.
type capResp struct {
	URL string `json:"url"`
}

// readCap fetches a FREE presigned GET URL for ref (reads cost no tokens -- this is what lets a device
// restore its wallet/data without first holding a token). writeCap fetches a presigned PUT URL, which the
// gateway grants only if ref already has a live lease. Both target the gateway's managed store, so a
// managed device uses them instead of store credentials and thus holds NO R2 creds (least-knowledge).
func (g *gatewayClient) readCap(ref string) (string, error) {
	var c capResp
	err := g.getJSON("/cap?op=read&ref="+url.QueryEscape(ref), &c)
	return c.URL, err
}

func (g *gatewayClient) writeCap(ref string) (string, error) {
	var c capResp
	err := g.getJSON("/cap?op=write&ref="+url.QueryEscape(ref), &c)
	return c.URL, err
}

type capsResp struct {
	Caps       map[string]string `json:"caps"`
	TTLSeconds int64             `json:"ttl_seconds"`
}

// readCaps is the BATCH of readCap: one POST returns free presigned GET URLs for many refs (+ their shared
// lifetime), so a restore prefetches all its chunk caps in a handful of round-trips instead of one per chunk
// (#71 step 2). An old gateway without /caps returns non-200 -> the caller falls back to per-chunk readCap.
func (g *gatewayClient) readCaps(refs []string) (map[string]string, int64, error) {
	var c capsResp
	code, err := g.postJSON("/caps", map[string]any{"op": "read", "refs": refs}, &c)
	if err != nil {
		return nil, 0, err
	}
	if code != http.StatusOK {
		return nil, 0, fmt.Errorf("/caps: HTTP %d", code) // e.g. 404 on an old gateway -> caller falls back per-chunk
	}
	return c.Caps, c.TTLSeconds, nil
}

var errInsufficientCredit = errors.New("billing: insufficient credit in wallet")

// owedTokens computes how many tokens the gateway will charge for refs, mirroring its server-side math
// (ceil(total bytes / bytes-per-token), at least one). Lets the device present exactly what's owed.
func owedTokens(q quoteInfo, refs map[string]int64) int {
	var total int64
	for _, sz := range refs {
		if sz > 0 {
			total += sz
		}
	}
	per := q.GBPerToken * giB
	if per <= 0 {
		per = giB
	}
	owed := int(math.Ceil(float64(total) / float64(per)))
	if owed == 0 {
		owed = 1 // any data kept alive costs at least one token
	}
	return owed
}

// leaseRefs pays rent to keep refs alive: quote -> compute owed -> take that many tokens from the wallet
// (zero-knowledge first, faucet to top up) -> redeem -> persist the reduced wallet. The wallet on disk is
// only mutated on a successful redemption; a network/HTTP failure leaves it untouched so no tokens are lost.
func leaseRefs(g *gatewayClient, walletPath string, refs map[string]int64) (leaseInfo, error) {
	q, err := g.quote()
	if err != nil {
		return leaseInfo{}, err
	}
	w, err := loadWallet(walletPath)
	if err != nil {
		return leaseInfo{}, err
	}
	owed := owedTokens(q, refs)
	plain, blind, ok := w.choosePayment(owed)
	if !ok {
		return leaseInfo{}, errInsufficientCredit
	}
	info, code, err := g.lease(plain, blind, refs)
	if err != nil {
		return leaseInfo{}, err
	}
	if code != http.StatusOK {
		return leaseInfo{}, fmt.Errorf("lease: HTTP %d", code)
	}
	// Delta-lease (DIA-20260627): the gateway charges only refs not already covered through the target epoch,
	// so it may spend FEWER tokens than we presented. It consumes the first info.Spent of (plain ++ blind);
	// settlePayment returns the rest so re-leasing an already-covered footprint doesn't burn tokens.
	w.settlePayment(plain, blind, info.Spent)
	if err := saveWallet(walletPath, w); err != nil {
		return info, fmt.Errorf("lease ok but wallet save failed: %w", err)
	}
	return info, nil
}

// profileFootprint returns the refs a profile must keep leased -- its (vault) head, the CDC manifest blob, and
// every chunk -- each with its stored size (the gateway leases per ref by size). Handles the vault head
// (current) and the legacy bare-manifest head. Defensive: an unopenable/odd head leases just what it can and
// never panics. (DIA-20260626-B; was the stale item-Manifest model.)
func profileFootprint(base, profile, pass string) map[string]int64 {
	hk := headKey(base, profile, pass) // #80: the live head key (v2 if migrated, else legacy) -- resolve once
	head := getRef(base, hk)
	if head == "" {
		return nil // unknown profile or wrong passphrase
	}
	hblob := getBlob(base, head)
	// Lease the OBJECT KEYS the device actually reads/writes and the gateway presigns/GCs -- blob/<hash> for
	// content blobs, ref/<name> for the mutable head pointer -- NOT bare hashes. A bare-hash lease never
	// matched the device's blob/<hash> objects, so the gateway GC couldn't find them. The head pointer is
	// included (nominal size 0) so it gets a write cap in cap mode; size 0 rounds into the per-GiB token math.
	refs := map[string]int64{
		"blob/" + head: int64(len(hblob)),
		"ref/" + hk:    0,
	}
	if v, isV := parseVault(hblob); isV {
		dk := unwrapDK(v, "pass", deriveKey(profile, pass))
		if dk == nil {
			dk = seDK(v, profile, pass) // hardened identity: open via the device's secure-element slot
		}
		if dk == nil || v.Manifest == "" {
			return refs // can't open the manifest -> lease just the head (defensive)
		}
		addManifestChunks(base, dk, v.Manifest, refs) // the LIVE head's chunks
		// #58: also lease the last-K retained snapshots' chunks (+ each snapshot manifest blob), so a rollback
		// target's data survives the store GC. The refs map is content-addressed, so chunks shared with the
		// live head collapse -> only the accumulated DELTA across the retained versions actually costs.
		for _, s := range v.History {
			addManifestChunks(base, dk, s.Manifest, refs)
		}
		return refs
	}
	// legacy: the head blob IS the sealed manifest (pre-vault; no snapshot history)
	func() {
		defer func() { recover() }() // a bad/legacy manifest must never panic the footprint
		addCDCChunks(base, unseal(deriveKey(profile, pass), hblob), refs)
	}()
	return refs
}

// addManifestChunks fetches a sealed CDC manifest by hash, then leases the manifest blob + every chunk it
// references (keyed by object key -> size). Used for the live head and each retained snapshot (#58).
// Best-effort: a bad/legacy/unreadable manifest is skipped, never panics the footprint.
func addManifestChunks(base string, dk []byte, manifestHash string, refs map[string]int64) {
	defer func() { recover() }()
	if manifestHash == "" {
		return
	}
	mb := getBlob(base, manifestHash)
	refs["blob/"+manifestHash] = int64(len(mb))
	addCDCChunks(base, unseal(dk, mb), refs)
}

// addCDCChunks adds every chunk of a decrypted CDC manifest to refs. #85: prefer the per-chunk sizes carried
// in the manifest (local, instant); only a legacy pre-#85 manifest (no Sizes) falls back to blobSize -- which
// in cap mode DOWNLOADS each chunk.
func addCDCChunks(base string, pt []byte, refs map[string]int64) {
	if !bytes.HasPrefix(pt, cdcMagic) {
		return
	}
	var m chunkManifest
	if json.Unmarshal(pt[len(cdcMagic):], &m) != nil {
		return
	}
	haveSizes := len(m.Sizes) == len(m.Chunks)
	for i, ch := range m.Chunks {
		if ch == "" {
			continue
		}
		if haveSizes {
			refs["blob/"+ch] = m.Sizes[i]
		} else {
			refs["blob/"+ch] = blobSize(base, ch)
		}
	}
}

// ---- wallet roaming (DIA-20260627-B): the wallet seals/restores with the profile so the token balance
// survives the power-off wipe. It rides a distinct unguessable sub-ref (not the heavy data head), is
// fetched at login via a free read, and re-published whenever it changes. The store/gateway only ever
// hold ciphertext. ----

// walletHeadKey resolves where an identity's roaming wallet head CURRENTLY lives -- v2 if migrated, else the
// legacy sha256 ref. The wallet is a distinct unguessable sub-ref of the profile (name+"#wallet", bound to
// BOTH name and passphrase, so a wrong passphrase finds nothing: blind, like the data head); it's just another
// head for that sub-identity, so headKey/putHead handle its v2 migration + tombstoning for free. (#80)
func walletHeadKey(base, name, pass string) string { return headKey(base, name+"#wallet", pass) }

// resolveDK opens a profile's data key the way the footprint does: a vault head unwraps DK via the
// passphrase slot (or the device's secure-element slot for a hardened identity); a legacy bare head
// derives the key directly. Returns nil for an unknown profile or a wrong passphrase. The wallet seals
// under this same key, so it's reachable by exactly whoever can open the data.
func resolveDK(base, name, pass string) []byte {
	head := getRef(base, headKey(base, name, pass))
	if head == "" {
		return nil
	}
	hblob := getBlob(base, head)
	if v, isV := parseVault(hblob); isV {
		if dk := unwrapDK(v, "pass", deriveKey(name, pass)); dk != nil {
			return dk
		}
		return seDK(v, name, pass)
	}
	return deriveKey(name, pass)
}

// restoreWallet fetches the roaming wallet for (name,pass) and writes it to walletPath, unsealed under dk.
// A missing head means no wallet has roamed yet (first login / fresh faucet) -> walletPath is left absent
// and loadWallet later yields an empty wallet. An unreadable blob is treated the same (start empty rather
// than fail login) so a corrupt store can't lock a user out of their session.
func restoreWallet(base, name, pass string, dk []byte, walletPath string) error {
	head := getRef(base, walletHeadKey(base, name, pass))
	if head == "" {
		return nil
	}
	blob := getBlob(base, head)
	if len(blob) == 0 {
		return nil
	}
	var pt []byte
	func() {
		defer func() { recover() }() // a bad/corrupt wallet blob must never panic login
		pt = unseal(dk, blob)
	}()
	if len(pt) == 0 {
		return nil
	}
	tmp := walletPath + ".tmp"
	if err := os.WriteFile(tmp, pt, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, walletPath)
}

// sealWallet seals the wallet at walletPath under dk and publishes it, updating the wallet head ref. A
// missing walletPath (nothing minted yet) is a no-op. Returns the new head's content hash.
func sealWallet(base, name, pass string, dk []byte, walletPath string) (string, error) {
	if capMode() {
		return "", nil // managed mode: the wallet rides the seal flush (capFlush fold), not a standalone write
	}
	data, err := os.ReadFile(walletPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	h := postBlob(base, seal(dk, data))
	putHead(base, name+"#wallet", pass, h) // #80: migrates a legacy wallet ref to v2 + tombstones the legacy
	return h, nil
}

// freeRenewedThroughEpoch throttles free-tier (#54) lease renewal to once per epoch per process, so a free
// session renews its 90-day window when it first becomes due each epoch rather than on every keep-alive
// cycle. A fresh login starts a new process at -1, so renewal always runs on access. Reset in tests.
var freeRenewedThroughEpoch int64 = -1

// payRent keeps a profile's data alive for the current lease window and is safe to call on every sync: it
// only spends once the window has lapsed. Flow: quote the current epoch; if the wallet's recorded
// paid-through still covers it, do nothing. Otherwise resolve the footprint (the profile's data refs PLUS
// the wallet's own previous-epoch blob, so the wallet itself stays alive), redeem the owed tokens, record
// the new paid-through epoch, and re-seal the reduced wallet back to the store. Returns the lease info and
// whether rent was actually paid.
func payRent(g *gatewayClient, base, name, pass, walletPath string) (leaseInfo, bool, error) {
	q, err := g.quote()
	if err != nil {
		return leaseInfo{}, false, err
	}
	w, err := loadWallet(walletPath)
	if err != nil {
		return leaseInfo{}, false, err
	}
	if w.PaidThrough > q.CurrentEpoch {
		return leaseInfo{}, false, nil // paid window still covers the epoch -> nothing due
	}
	// A session with no spendable credit is on the FREE tier (#54): the gateway renews its lease for free
	// (metered per profile) and PaidThrough is never advanced, so renewal recurs every due cycle. Throttle
	// it to once per epoch per process so we don't recompute the footprint on every keep-alive; a fresh login
	// starts the process at -1, so renewal always runs on access.
	freeUser := w.balance() == 0
	if freeUser {
		if freeRenewedThroughEpoch >= q.CurrentEpoch {
			return leaseInfo{}, false, nil
		}
		freeRenewedThroughEpoch = q.CurrentEpoch
	}
	dk := resolveDK(base, name, pass)
	if dk == nil {
		return leaseInfo{}, false, fmt.Errorf("pay-rent: cannot open profile %q (unknown or wrong passphrase)", name)
	}
	refs := profileFootprint(base, name, pass)
	if len(refs) == 0 {
		return leaseInfo{}, false, nil // nothing stored yet -> nothing to keep alive
	}
	whk := walletHeadKey(base, name, pass) // #80: the live wallet head key (v2 or legacy) -- resolve once
	if cur := getRef(base, whk); cur != "" {
		refs["blob/"+cur] = blobSize(base, cur) // keep the wallet's own (prior-epoch) blob alive through this lease
	}
	refs["ref/"+whk] = 0 // and its mutable pointer (so it gets a write cap in cap mode)
	if freeUser {
		// Free renewal: a token-less lease that pushes the 90-day window forward without spending. No
		// PaidThrough advance, so the next epoch renews again. Over quota / disabled -> the user needs credit.
		finfo, code, ferr := g.leaseFree(profileRefV2(name, pass), refs) // #80: stable per-profile metering key
		if ferr != nil {
			return leaseInfo{}, false, ferr
		}
		if code != http.StatusOK {
			return leaseInfo{}, false, fmt.Errorf("pay-rent: free tier unavailable for %q (HTTP %d) -- add credit", name, code)
		}
		_, err := sealWallet(base, name, pass, dk, walletPath) // no-op in cap mode; publishes the wallet otherwise
		return finfo, true, err
	}
	info, err := leaseRefs(g, walletPath, refs)
	if err != nil {
		return leaseInfo{}, false, err
	}
	// record paid-through on the now-reduced wallet and re-seal so the roamed copy reflects the spend.
	w, err = loadWallet(walletPath)
	if err != nil {
		return info, true, err
	}
	w.PaidThrough = info.ThroughEpoch
	if err := saveWallet(walletPath, w); err != nil {
		return info, true, err
	}
	_, err = sealWallet(base, name, pass, dk, walletPath)
	return info, true, err
}
