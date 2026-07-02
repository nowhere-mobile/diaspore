package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockCapStore stands in for the gateway's /cap endpoint + the presigned object store it points at: GET
// /cap?ref=K returns a URL to /obj?k=K, and /obj serves (GET) / accepts (PUT) the bytes for that key. It
// mirrors the real flow (cap fetch -> presigned GET/PUT) so the device's cap-mode I/O is exercised without
// any store credentials. Returns the server + its backing object map (so tests can seed/inspect objects).
func mockCapStore(t *testing.T) (*httptest.Server, map[string][]byte, *sync.Mutex) {
	t.Helper()
	objs := map[string][]byte{}
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/obj", func(w http.ResponseWriter, r *http.Request) {
		k := r.URL.Query().Get("k")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			objs[k] = b
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			mu.Lock()
			b, ok := objs[k]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// ServeContent honors Range (206 + Content-Range) like a real S3/R2 object -> exercises capSize's
			// size-only ranged GET (#87); a plain full GET (capGet) still gets the whole body.
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(b))
		}
	})
	mux.HandleFunc("/cap", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("ref")
		json.NewEncoder(w).Encode(capResp{URL: "http://" + r.Host + "/obj?k=" + url.QueryEscape(ref)})
	})
	// Billing endpoints (so write tests can fund a wallet + batch-lease): quote, dev faucet, lease-with-caps.
	mux.HandleFunc("GET /quote", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(quoteInfo{Denom: "1GBmo", GBPerToken: 1, EpochSeconds: 604800, EpochsPerLease: 4, CurrentEpoch: 42})
	})
	var counter int
	mux.HandleFunc("POST /tokens", func(w http.ResponseWriter, r *http.Request) {
		var req tokensReq
		json.NewDecoder(r.Body).Decode(&req)
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
			caps[ref] = "http://" + r.Host + "/obj?k=" + url.QueryEscape(ref) // write cap -> the same object store
		}
		if req.Free { // free tier (#54): no tokens, metered against a per-profile quota (5 GiB here)
			if req.Profile == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if total > 5*giB { // over quota -> device must fall back to paid
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
		json.NewEncoder(w).Encode(leaseInfo{ThroughEpoch: 46, Spent: owed, WriteCaps: caps})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, objs, &mu
}

// TestCapSizeRanged (#87): in cap mode, blobSize must size a blob via a 1-byte ranged GET (Content-Range),
// NOT by downloading the whole chunk. Assert capSize + blobSize return the true size against a Range-capable
// mock store.
func TestCapSizeRanged(t *testing.T) {
	srv, objs, mu := mockCapStore(t)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "") // no creds -> capMode()
	if !capMode() {
		t.Fatal("expected cap mode")
	}
	data := bytes.Repeat([]byte("blobdata "), 5000) // 40000 bytes
	mu.Lock()
	objs["blob/deadbeef"] = data
	mu.Unlock()
	if got := capSize("blob/deadbeef"); got != int64(len(data)) {
		t.Fatalf("capSize = %d, want %d", got, len(data))
	}
	if got := blobSize("s3", "deadbeef"); got != int64(len(data)) {
		t.Fatalf("blobSize(cap) = %d, want %d", got, len(data))
	}
	if capSize("blob/missing") != -1 {
		t.Fatal("capSize of a missing blob should be -1 (absent -> blobSize falls back)")
	}
}

// TestCapModeWrites (DIA-20260627, P3): in cap mode a bracketed batch of postBlob/putRef buffers, and a
// single capFlush batch-leases (spends one token) + PUTs every object via its write cap -- no store creds.
func TestCapModeWrites(t *testing.T) {
	srv, objs, mu := mockCapStore(t)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "") // no creds -> capMode()
	capReadCache = map[string]capEntry{}

	gw := newGatewayClient(srv.URL)
	toks, err := gw.buyFaucet(10)
	if err != nil {
		t.Fatal(err)
	}
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	t.Setenv("NOWHERE_WALLET", wpath)
	if err := saveWallet(wpath, &Wallet{Tokens: toks}); err != nil {
		t.Fatal(err)
	}

	// a write outside a bracket must fail loudly (never a silent / unpaid write).
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("cap-mode write outside capBegin should fail")
			}
		}()
		postBlob("s3", []byte("unbracketed"))
	}()

	// bracketed batch: two blobs + a pointer, one flush.
	capBegin()
	h1 := postBlob("s3", []byte("chunk one"))
	h2 := postBlob("s3", bytes.Repeat([]byte("two "), 500))
	putRef("s3", "alice", h1)
	capFlush()

	mu.Lock()
	defer mu.Unlock()
	if !bytes.Equal(objs["blob/"+h1], []byte("chunk one")) || objs["blob/"+h2] == nil {
		t.Fatal("blobs were not written via caps")
	}
	if strings.TrimSpace(string(objs["ref/alice"])) != h1 {
		t.Fatalf("pointer not written via cap: %q", objs["ref/alice"])
	}
	if after, _ := loadWallet(wpath); after.balance() != 9 {
		t.Fatalf("expected one token spent on the batch lease, balance=%d", after.balance())
	}
}

// TestCapModeCreateVault (DIA-20260627, P3 integration): createVault brackets its writes internally, so in
// cap mode a brand-new vault is written entirely via caps (no store creds) for one batch payment, and the
// head pointer reads back via a free cap. (Exercises the bracket + the read+write round-trip end to end.)
func TestCapModeCreateVault(t *testing.T) {
	srv, _, _ := mockCapStore(t)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "")
	capReadCache = map[string]capEntry{}
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	t.Setenv("NOWHERE_WALLET", wpath)
	toks, err := newGatewayClient(srv.URL).buyFaucet(10)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWallet(wpath, &Wallet{Tokens: toks}); err != nil {
		t.Fatal(err)
	}

	name, pass := "capnew", "brand new managed pass"
	createVault("s3", name, pass) // brackets internally -> writes the vault via caps + spends

	if getRef("s3", profileRefV2(name, pass)) == "" {
		t.Fatal("createVault in cap mode did not publish the head pointer via a cap")
	}
	if after, _ := loadWallet(wpath); after.balance() >= 10 {
		t.Fatalf("createVault in cap mode spent no tokens (balance %d)", after.balance())
	}
}

// TestCapModeReads (DIA-20260627, P1): with GATEWAY_URL set and no S3 creds, getBlob/getRef route through
// free read caps -- the device reads with NO store credentials, and a missing ref reads as absent (blind).
func TestCapModeReads(t *testing.T) {
	srv, objs, mu := mockCapStore(t)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "") // no creds -> capMode()
	capReadCache = map[string]capEntry{}

	if !capMode() {
		t.Fatal("capMode should be true with GATEWAY_URL set and no S3 creds")
	}

	data := bytes.Repeat([]byte("cap blob "), 1000)
	mu.Lock()
	objs["blob/abc123"] = data
	objs["ref/alice"] = []byte("headhash999\n")
	mu.Unlock()

	if got := getBlob("s3", "abc123"); !bytes.Equal(got, data) {
		t.Fatalf("getBlob via cap: %d bytes, want %d", len(got), len(data))
	}
	if got := getRef("s3", "alice"); got != "headhash999" {
		t.Fatalf("getRef via cap = %q, want headhash999", got)
	}
	// a wrong/unknown ref must read as absent -> blind login.
	if got := getRef("s3", "nobody"); got != "" {
		t.Fatalf("missing ref should read empty, got %q", got)
	}
}

// TestCapModeReadRetriesBlip (#70): a transient failure on the gateway /cap fetch OR the presigned GET (the
// "software caused connection abort" that used to dump a mid-restore login back to the gate) is RETRIED, so a
// single blip costs a retry instead of the whole restore. A persistently-absent key is still a definitive miss
// (blind login), never spun on. Blips are simulated by hijacking + closing the connection (a real transport
// error, like a Wi-Fi drop), not a clean 5xx.
func TestCapModeReadRetriesBlip(t *testing.T) {
	old := s3RetryBase
	s3RetryBase = time.Millisecond // shrink the backoff so the test is fast
	defer func() { s3RetryBase = old }()

	objs := map[string][]byte{"blob/x": bytes.Repeat([]byte("y"), 4096)}
	var mu sync.Mutex
	var capHits, objHits int
	drop := func(w http.ResponseWriter) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		c, _, err := hj.Hijack()
		if err == nil {
			c.Close() // slam the connection -> the client's http.Get returns a transport error
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/cap", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capHits++
		n := capHits
		mu.Unlock()
		if n == 1 { // the very first cap fetch blips
			drop(w)
			return
		}
		ref := r.URL.Query().Get("ref")
		json.NewEncoder(w).Encode(capResp{URL: "http://" + r.Host + "/obj?k=" + url.QueryEscape(ref)})
	})
	mux.HandleFunc("/obj", func(w http.ResponseWriter, r *http.Request) {
		k := r.URL.Query().Get("k")
		mu.Lock()
		b, ok := objs[k]
		objHits++
		n := objHits
		mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound) // persistently absent -> a real miss, never a blip
			return
		}
		if n <= 2 { // the first two object GETs blip mid-flight
			drop(w)
			return
		}
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "") // no creds -> capMode()
	capReadCache = map[string]capEntry{}

	// 1 cap blip + 2 object blips, then success -> getBlob must still return the bytes (restore survives).
	if got := getBlob("s3", "x"); !bytes.Equal(got, objs["blob/x"]) {
		t.Fatalf("getBlob after transient blips: %d bytes, want %d", len(got), len(objs["blob/x"]))
	}
	// a genuinely-absent ref still reads empty (blind login), not a loud failure from the retry loop.
	if got := getRef("s3", "ghost"); got != "" {
		t.Fatalf("absent ref should read empty after retries, got %q", got)
	}
}

// TestCapModeFoldWallet (DIA-20260627, device fold): with a session set, capFlush re-seals the POST-spend
// wallet into the SAME lease as the data, so the wallet is written for free (rounds in) and roams reflecting
// the post-spend balance -- breaking the wallet self-reference without a free-write primitive.
func TestCapModeFoldWallet(t *testing.T) {
	srv, objs, mu := mockCapStore(t)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "")
	capReadCache = map[string]capEntry{}
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	t.Setenv("NOWHERE_WALLET", wpath)
	toks, err := newGatewayClient(srv.URL).buyFaucet(10)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveWallet(wpath, &Wallet{Tokens: toks}); err != nil {
		t.Fatal(err)
	}

	name, pass := "foldu", "fold the wallet free"
	dk := deriveKey(name, pass)
	capBegin()
	capSetSession(name, pass, dk)
	h := postBlob("s3", bytes.Repeat([]byte("d"), 2000)) // some data
	putRef("s3", profileRefV2(name, pass), h)              // a head pointer
	capFlush()

	mu.Lock()
	defer mu.Unlock()
	ptr := objs["ref/"+profileRefV2(name+"#wallet", pass)] // #80: the fold migrates the wallet to its v2 ref
	if ptr == nil {
		t.Fatal("wallet pointer was not folded into the seal flush")
	}
	wblob := objs["blob/"+strings.TrimSpace(string(ptr))]
	if wblob == nil {
		t.Fatal("wallet blob not written via the fold")
	}
	// only the data was charged (1 token); the wallet rode in free.
	if after, _ := loadWallet(wpath); after.balance() != 9 {
		t.Fatalf("expected 1 token spent (data only, wallet rode free), balance=%d", after.balance())
	}
	// the roamed wallet decodes to the POST-spend balance.
	var rw Wallet
	if err := json.Unmarshal(unseal(dk, wblob), &rw); err != nil {
		t.Fatalf("roamed wallet did not unseal: %v", err)
	}
	if rw.balance() != 9 {
		t.Fatalf("roamed wallet balance %d, want 9 (post-spend)", rw.balance())
	}
}

// TestCapModeFreeTier (#54): a session with NO wallet credit seals via the FREE tier -- capFlush falls back
// to a token-less leaseFree (metered per profileRef), writes every object via the returned caps, spends
// nothing, and still folds the wallet so it roams. This is what lets a credential-less free user roam.
func TestCapModeFreeTier(t *testing.T) {
	srv, objs, mu := mockCapStore(t)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "")
	capReadCache = map[string]capEntry{}
	blobCache = nil // reset the cross-test "known present" set; another test wrote the same content
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	t.Setenv("NOWHERE_WALLET", wpath)
	if err := saveWallet(wpath, &Wallet{}); err != nil { // empty wallet -> a free user (no tokens at all)
		t.Fatal(err)
	}

	name, pass := "freeu", "no credit just the free tier"
	dk := deriveKey(name, pass)
	capBegin()
	capSetSession(name, pass, dk)
	h := postBlob("s3", bytes.Repeat([]byte("d"), 2000)) // ~2 KB of data, well under the 5 GiB free quota
	putRef("s3", profileRefV2(name, pass), h)
	capFlush() // no tokens -> free fallback -> writes via free caps

	mu.Lock()
	defer mu.Unlock()
	if objs["blob/"+h] == nil {
		t.Fatal("free-tier data blob was not written via a free cap")
	}
	if objs["ref/"+profileRefV2(name, pass)] == nil {
		t.Fatal("free-tier head pointer was not written")
	}
	if after, _ := loadWallet(wpath); after.balance() != 0 {
		t.Fatalf("free lease must spend nothing, balance=%d", after.balance())
	}
	if objs["ref/"+profileRefV2(name+"#wallet", pass)] == nil { // #80: folded at the wallet's v2 ref
		t.Fatal("wallet was not folded into the free lease (won't roam)")
	}
}

// TestCapModeFreeOverQuota (#54): when the free quota is exceeded AND there's no credit, capFlush surfaces
// errInsufficientCredit (mapped to NOCREDIT by the handlers) rather than writing a half-paid seal.
func TestCapModeFreeOverQuota(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cap", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(capResp{URL: "http://" + r.Host + "/obj?k=" + url.QueryEscape(r.URL.Query().Get("ref"))})
	})
	mux.HandleFunc("GET /quote", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(quoteInfo{Denom: "1GBmo", GBPerToken: 1, EpochSeconds: 604800, EpochsPerLease: 4, CurrentEpoch: 42})
	})
	mux.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusPaymentRequired) // free over-quota AND no tokens -> 402 either way
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "")
	capReadCache = map[string]capEntry{}
	blobCache = nil
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	t.Setenv("NOWHERE_WALLET", wpath)
	if err := saveWallet(wpath, &Wallet{}); err != nil {
		t.Fatal(err)
	}

	name, pass := "overu", "free quota blown"
	capBegin()
	capSetSession(name, pass, deriveKey(name, pass))
	postBlob("s3", []byte("data over the free quota"))
	putRef("s3", profileRefV2(name, pass), "deadbeef")
	var got any
	func() {
		defer func() { got = recover() }()
		capFlush()
	}()
	if got != errInsufficientCredit {
		t.Fatalf("over-quota free flush should panic errInsufficientCredit, got %v", got)
	}
}

// TestStoreConfiguredCapMode (DIA-20260627, P4): a managed device (GATEWAY_URL set, no S3 creds) is a fully
// configured, login-able store -- the gateway IS the store -- even though it holds no S3 credentials.
func TestStoreConfiguredCapMode(t *testing.T) {
	t.Setenv("S3_ENDPOINT", "")
	t.Setenv("S3_ACCESS_KEY", "")
	t.Setenv("GATEWAY_URL", "https://api.nowhere.mobile")
	if !capMode() {
		t.Fatal("capMode should be true (gateway set, no creds)")
	}
	if !storeConfigured() {
		t.Fatal("storeConfigured should be true in cap mode")
	}
	// neither a gateway nor creds -> not configured (gate -> Settings).
	t.Setenv("GATEWAY_URL", "")
	if storeConfigured() {
		t.Fatal("storeConfigured should be false with no gateway and no creds")
	}
}

// TestCapFlushAtomicHead (#45): a FAILED blob PUT must abort the flush BEFORE the head pointer is written, so
// a partial / crashed seal can NEVER clobber a good head (the a/a1234 data-loss bug). Pointers are PUT last.
func TestCapFlushAtomicHead(t *testing.T) {
	objs := map[string][]byte{}
	var mu sync.Mutex
	var failKey string
	mux := http.NewServeMux()
	mux.HandleFunc("/obj", func(w http.ResponseWriter, r *http.Request) {
		k := r.URL.Query().Get("k")
		if r.Method == http.MethodPut {
			mu.Lock()
			fk := failKey
			mu.Unlock()
			if k == fk {
				w.WriteHeader(http.StatusInternalServerError) // simulate a mid-flush write failure / crash
				return
			}
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			objs[k] = b
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/cap", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(capResp{URL: "http://" + r.Host + "/obj?k=" + url.QueryEscape(r.URL.Query().Get("ref"))})
	})
	mux.HandleFunc("GET /quote", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(quoteInfo{Denom: "1GBmo", GBPerToken: 1, EpochSeconds: 604800, EpochsPerLease: 4, CurrentEpoch: 42})
	})
	mux.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		json.NewDecoder(r.Body).Decode(&req)
		caps := map[string]string{}
		for ref := range req.Refs {
			caps[ref] = "http://" + r.Host + "/obj?k=" + url.QueryEscape(ref)
		}
		json.NewEncoder(w).Encode(leaseInfo{ThroughEpoch: 46, Spent: 1, WriteCaps: caps})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("GATEWAY_URL", srv.URL)
	t.Setenv("S3_ACCESS_KEY", "")
	capReadCache = map[string]capEntry{}
	wpath := filepath.Join(t.TempDir(), "wallet.json")
	t.Setenv("NOWHERE_WALLET", wpath)
	var toks []Token
	for i := 0; i < 10; i++ {
		toks = append(toks, Token{Denom: "1GBmo", Nonce: "n" + strconv.Itoa(i), Exp: 1 << 62, Sig: "s"})
	}
	if err := saveWallet(wpath, &Wallet{Tokens: toks}); err != nil {
		t.Fatal(err)
	}

	capBegin()
	postBlob("s3", []byte("good blob"))
	h2 := postBlob("s3", []byte("doomed blob"))
	putRef("s3", "head", h2) // the head pointer -- must NOT be written when a blob PUT fails
	mu.Lock()
	failKey = "blob/" + h2
	mu.Unlock()
	func() {
		defer func() { _ = recover() }() // the failing blob PUT calls fail() -> panic; expected
		capFlush()
	}()

	mu.Lock()
	defer mu.Unlock()
	if _, ok := objs["ref/head"]; ok {
		t.Fatal("head pointer was written despite a failed blob PUT -- a partial seal could clobber a good head")
	}
}
