package main

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// mockGateway mirrors the nowhere-cloud gateway's wire contract (the real server is tested in that repo).
// It mints faucet tokens, tracks spent nonces, and charges ceil(total/GiB) tokens per lease.
func mockGateway(t *testing.T) *httptest.Server {
	t.Helper()
	spent := map[string]bool{}
	var counter int
	mux := http.NewServeMux()

	mux.HandleFunc("GET /quote", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(quoteInfo{
			Denom: "1GBmo", GBPerToken: 1, EpochSeconds: 604800, EpochsPerLease: 4,
			PriceCents: 200, CurrentEpoch: 42, IssuerPubKey: "testpub",
		})
	})

	mux.HandleFunc("POST /tokens", func(w http.ResponseWriter, r *http.Request) {
		var req tokensReq
		json.NewDecoder(r.Body).Decode(&req)
		if req.Payment != "dev-faucet" {
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		out := tokensResp{}
		for i := 0; i < req.Count; i++ {
			counter++
			out.Tokens = append(out.Tokens, Token{Denom: "1GBmo", Nonce: "n" + strconv.Itoa(counter), Exp: 1 << 62, Sig: "sig"})
		}
		json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		json.NewDecoder(r.Body).Decode(&req)
		var total int64
		for _, sz := range req.Refs {
			total += sz
		}
		caps := map[string]string{}
		for ref := range req.Refs {
			caps[ref] = "https://store.test/" + ref + "?sig=x"
		}
		if req.Free { // free tier (#54): no tokens, metered per profile against a 5 GiB quota here
			if total > 5*giB {
				w.WriteHeader(http.StatusPaymentRequired)
				return
			}
			json.NewEncoder(w).Encode(leaseInfo{ThroughEpoch: 99, Spent: 0, WriteCaps: caps})
			return
		}
		owed := int(math.Ceil(float64(total) / float64(giB)))
		if owed == 0 {
			owed = 1
		}
		if len(req.Tokens) < owed {
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		for _, tok := range req.Tokens[:owed] {
			if spent[tok.Nonce] {
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		for _, tok := range req.Tokens[:owed] {
			spent[tok.Nonce] = true
		}
		json.NewEncoder(w).Encode(leaseInfo{ThroughEpoch: 46, Spent: owed, WriteCaps: caps})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWalletRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wallet.json")
	w, err := loadWallet(path) // missing file -> empty wallet
	if err != nil || w.balance() != 0 {
		t.Fatalf("empty load: bal=%d err=%v", w.balance(), err)
	}
	w.Tokens = append(w.Tokens, Token{Nonce: "a"}, Token{Nonce: "b"}, Token{Nonce: "c"})
	if err := saveWallet(path, w); err != nil {
		t.Fatal(err)
	}
	got, err := loadWallet(path)
	if err != nil || got.balance() != 3 {
		t.Fatalf("reload: bal=%d err=%v", got.balance(), err)
	}
	taken, ok := got.take(2)
	if !ok || len(taken) != 2 || got.balance() != 1 {
		t.Fatalf("take(2): ok=%v taken=%d remaining=%d", ok, len(taken), got.balance())
	}
	if _, ok := got.take(5); ok {
		t.Fatal("take(5) on a 1-token wallet should fail")
	}
}

func TestBillingLine(t *testing.T) {
	wp := filepath.Join(t.TempDir(), "wallet.json")

	// no wallet on disk -> nothing to show
	if got := billingLine(wp); got != "NONE\n" {
		t.Errorf("no wallet: got %q, want NONE", got)
	}

	// credit + a paid-through epoch -> the subscription line
	if err := saveWallet(wp, &Wallet{Tokens: []Token{{}, {}, {}}, PaidThrough: 2951}); err != nil {
		t.Fatal(err)
	}
	if got, want := billingLine(wp), "OK credit=3 through=2951 epochsec=604800\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// empty wallet (no tokens, never leased) -> NONE
	if err := saveWallet(wp, &Wallet{}); err != nil {
		t.Fatal(err)
	}
	if got := billingLine(wp); got != "NONE\n" {
		t.Errorf("empty wallet: got %q, want NONE", got)
	}
}

func TestOwedTokens(t *testing.T) {
	q := quoteInfo{GBPerToken: 1}
	cases := []struct {
		total int64
		want  int
	}{
		{0, 1}, {1, 1}, {giB, 1}, {giB + 1, 2}, {3 * giB, 3}, {3*giB - 1, 3},
	}
	for _, c := range cases {
		if got := owedTokens(q, map[string]int64{"r": c.total}); got != c.want {
			t.Errorf("owed(%d) = %d, want %d", c.total, got, c.want)
		}
	}
}

func TestBuyFaucetAndLease(t *testing.T) {
	srv := mockGateway(t)
	gw := newGatewayClient(srv.URL)
	path := filepath.Join(t.TempDir(), "wallet.json")

	// buy 3 tokens into the wallet.
	toks, err := gw.buyFaucet(3)
	if err != nil || len(toks) != 3 {
		t.Fatalf("buyFaucet: n=%d err=%v", len(toks), err)
	}
	w := &Wallet{Tokens: toks}
	if err := saveWallet(path, w); err != nil {
		t.Fatal(err)
	}

	// lease 1.5 GiB across two refs -> owed 2 -> wallet drops to 1.
	refs := map[string]int64{"refA": giB, "refB": giB / 2}
	info, err := leaseRefs(gw, path, refs)
	if err != nil {
		t.Fatalf("leaseRefs: %v", err)
	}
	if info.Spent != 2 || info.ThroughEpoch != 46 {
		t.Fatalf("lease info: spent=%d through=%d", info.Spent, info.ThroughEpoch)
	}
	if len(info.WriteCaps) != 2 {
		t.Fatalf("write caps: %+v", info.WriteCaps)
	}
	after, _ := loadWallet(path)
	if after.balance() != 1 {
		t.Fatalf("wallet after lease: %d, want 1", after.balance())
	}
}

// TestProfileFootprintCDC (DIA-20260626-B): the footprint of a real CDC/vault profile spans its head, the CDC
// manifest blob, and every chunk -- each with a positive stored size -- and leases cleanly end-to-end.
func TestProfileFootprintCDC(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "fp", "footprint horse battery staple"
	createVault(base, name, pass)

	src := t.TempDir()
	big := bytes.Repeat([]byte("footprint chunk data 0123456789 abcdefghij\n"), 200000) // ~8.6MB -> several chunks
	if err := os.WriteFile(filepath.Join(src, "f.txt"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)

	refs := profileFootprint(base, name, pass)
	if len(refs) < 3 {
		t.Fatalf("expected head + manifest + >=1 chunk, got %d refs", len(refs))
	}
	// refs are full OBJECT KEYS: blob/<hash> for content blobs, ref/<name> for the mutable head pointer.
	head := getRef(base, headKey(base, name, pass))
	if refs["blob/"+head] <= 0 {
		t.Fatalf("head blob (blob/%s) missing / zero size in footprint", head)
	}
	if _, ok := refs["ref/"+headKey(base, name, pass)]; !ok {
		t.Fatal("mutable head pointer ref/<profileRef> not leased in footprint")
	}
	var total int64
	for ref, sz := range refs {
		if sz < 0 {
			t.Fatalf("ref %s has negative size %d", ref, sz)
		}
		total += sz
	}
	if total <= 0 {
		t.Fatal("footprint total (blob) size is zero")
	}

	// End-to-end: the footprint leases cleanly (footprint -> owed -> redeem -> a write cap per ref).
	srv := mockGateway(t)
	gw := newGatewayClient(srv.URL)
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	toks, err := gw.buyFaucet(owedTokens(quoteInfo{GBPerToken: 1}, refs) + 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWallet(wpath, &Wallet{Tokens: toks}); err != nil {
		t.Fatal(err)
	}
	info, err := leaseRefs(gw, wpath, refs)
	if err != nil {
		t.Fatalf("leaseRefs on footprint: %v", err)
	}
	if len(info.WriteCaps) != len(refs) {
		t.Fatalf("write caps %d != refs %d", len(info.WriteCaps), len(refs))
	}
}

func TestLeaseInsufficientCreditLeavesWallet(t *testing.T) {
	srv := mockGateway(t)
	gw := newGatewayClient(srv.URL)
	path := filepath.Join(t.TempDir(), "wallet.json")

	// one token, but 3 GiB needs three -> insufficient, and the wallet must be untouched.
	if err := saveWallet(path, &Wallet{Tokens: []Token{{Nonce: "only"}}}); err != nil {
		t.Fatal(err)
	}
	_, err := leaseRefs(gw, path, map[string]int64{"big": 3 * giB})
	if err == nil || err != errInsufficientCredit {
		t.Fatalf("want errInsufficientCredit, got %v", err)
	}
	after, _ := loadWallet(path)
	if after.balance() != 1 {
		t.Fatalf("wallet should be untouched: %d", after.balance())
	}
}

// TestWalletRoamRoundTrip (DIA-20260627-B): the wallet seals/restores under the profile's data key, so the
// token balance and paid-through epoch survive a power-off wipe; a wrong passphrase finds no roaming wallet.
func TestWalletRoamRoundTrip(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "wlt", "roam these tokens please"
	createVault(base, name, pass)
	dk := resolveDK(base, name, pass)
	if dk == nil {
		t.Fatal("resolveDK returned nil for a valid profile")
	}

	src := filepath.Join(t.TempDir(), "wallet.json")
	if err := saveWallet(src, &Wallet{Tokens: []Token{{Nonce: "a"}, {Nonce: "b"}}, PaidThrough: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := sealWallet(base, name, pass, dk, src); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "wallet.json")
	if err := restoreWallet(base, name, pass, dk, dst); err != nil {
		t.Fatal(err)
	}
	got, _ := loadWallet(dst)
	if got.balance() != 2 || got.PaidThrough != 7 {
		t.Fatalf("roamed wallet: bal=%d paid=%d, want bal=2 paid=7", got.balance(), got.PaidThrough)
	}

	// a wrong passphrase derives a different wallet ref -> blind: nothing to restore, file stays absent.
	bad := filepath.Join(t.TempDir(), "wallet.json")
	if err := restoreWallet(base, name, "wrong pass", dk, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("wrong-pass restore should find no roaming wallet")
	}
}

// TestPayRentRoamsAndIsIdempotent (DIA-20260627-B): payRent leases a real profile's footprint, records
// paid-through, and roams the reduced wallet; a second call in the same epoch spends nothing.
func TestPayRentRoamsAndIsIdempotent(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "rent", "keep my bytes alive please"
	createVault(base, name, pass)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), bytes.Repeat([]byte("rent data 0123456789\n"), 100000), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)

	srv := mockGateway(t) // quote: current epoch 42, lease through 46
	gw := newGatewayClient(srv.URL)
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	toks, err := gw.buyFaucet(10)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWallet(wpath, &Wallet{Tokens: toks}); err != nil {
		t.Fatal(err)
	}

	info, paid, err := payRent(gw, base, name, pass, wpath)
	if err != nil || !paid {
		t.Fatalf("payRent #1: paid=%v err=%v", paid, err)
	}
	if info.ThroughEpoch != 46 {
		t.Fatalf("through epoch %d, want 46", info.ThroughEpoch)
	}
	w1, _ := loadWallet(wpath)
	if w1.PaidThrough != 46 {
		t.Fatalf("paid-through not recorded: %d", w1.PaidThrough)
	}
	if w1.balance() >= 10 {
		t.Fatalf("no tokens were spent: %d", w1.balance())
	}

	// second call in the same epoch (42 < 46) is a no-op: no further spend.
	_, paid2, err := payRent(gw, base, name, pass, wpath)
	if err != nil || paid2 {
		t.Fatalf("payRent #2 should be a no-op: paid=%v err=%v", paid2, err)
	}
	w2, _ := loadWallet(wpath)
	if w2.balance() != w1.balance() {
		t.Fatalf("idempotent call spent tokens: %d -> %d", w1.balance(), w2.balance())
	}

	// the wallet roamed: a fresh device restores the reduced balance + paid-through.
	dk := resolveDK(base, name, pass)
	fresh := filepath.Join(t.TempDir(), "wallet.json")
	if err := restoreWallet(base, name, pass, dk, fresh); err != nil {
		t.Fatal(err)
	}
	wr, _ := loadWallet(fresh)
	if wr.balance() != w1.balance() || wr.PaidThrough != 46 {
		t.Fatalf("roamed wallet: bal=%d paid=%d, want bal=%d paid=46", wr.balance(), wr.PaidThrough, w1.balance())
	}
}

// TestPayRentFreeRenew (#54, slice 2): a session with NO credit renews its lease on the FREE tier --
// payRent calls leaseFree, spends nothing, never advances PaidThrough, and is throttled to once per epoch
// per process so it doesn't re-lease every keep-alive cycle (renew-on-access).
func TestPayRentFreeRenew(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "freerent", "no tokens just the free tier"
	createVault(base, name, pass)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), bytes.Repeat([]byte("free data\n"), 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	pushProfile(base, name, pass, src)

	srv := mockGateway(t) // quote: current epoch 42; a free lease comes back through 99
	gw := newGatewayClient(srv.URL)
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	if err := saveWallet(wpath, &Wallet{}); err != nil { // empty wallet -> a free user (no tokens)
		t.Fatal(err)
	}
	freeRenewedThroughEpoch = -1 // reset the per-process throttle

	info, paid, err := payRent(gw, base, name, pass, wpath)
	if err != nil || !paid {
		t.Fatalf("free payRent #1: paid=%v err=%v", paid, err)
	}
	if info.ThroughEpoch != 99 {
		t.Fatalf("free lease through %d, want 99", info.ThroughEpoch)
	}
	w1, _ := loadWallet(wpath)
	if w1.PaidThrough != 0 {
		t.Fatalf("free renewal must NOT advance PaidThrough, got %d", w1.PaidThrough)
	}
	if w1.balance() != 0 {
		t.Fatalf("free renewal must spend nothing, balance=%d", w1.balance())
	}

	// second call in the same epoch is throttled -> a no-op (no re-lease, no error).
	if _, paid2, err := payRent(gw, base, name, pass, wpath); err != nil || paid2 {
		t.Fatalf("free payRent #2 should be a throttled no-op: paid=%v err=%v", paid2, err)
	}
}

// TestLeaseRestoresUnspentTokens (DIA-20260627, primitive A device half): when the gateway charges fewer
// tokens than presented (delta lease -- some refs already covered), the device returns the unspent tokens to
// the wallet rather than burning them.
func TestLeaseRestoresUnspentTokens(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /quote", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(quoteInfo{Denom: "1GBmo", GBPerToken: 1, CurrentEpoch: 42})
	})
	mux.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(leaseInfo{ThroughEpoch: 46, Spent: 1, WriteCaps: map[string]string{}}) // charge 1 only
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "wallet.json")
	if err := saveWallet(path, &Wallet{Tokens: []Token{{Nonce: "a"}, {Nonce: "b"}, {Nonce: "c"}, {Nonce: "d"}, {Nonce: "e"}}}); err != nil {
		t.Fatal(err)
	}
	// 3 GiB -> device owes 3 -> takes 3 -> gateway spends 1 -> restores 2 -> 5-1 = 4 left.
	if _, err := leaseRefs(newGatewayClient(srv.URL), path, map[string]int64{"r": 3 * giB}); err != nil {
		t.Fatal(err)
	}
	if after, _ := loadWallet(path); after.balance() != 4 {
		t.Fatalf("delta lease: balance %d, want 4 (spent 1 of 3 taken, restored 2)", after.balance())
	}
}
