package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"nowhereagent/blindrsa"
)

// mockSubGateway stands in for the gateway's blind-voucher endpoints: /issue/pubkey, /sub/voucher (budget-
// gated per (subHash, epoch)), and /refill (verify + burn vouchers, blind-sign one token each). It blind-signs
// with the same library the device uses, so the device's Finalize+Verify exercises the real round-trip.
func mockSubGateway(t *testing.T, priv *rsa.PrivateKey, subHash string, epoch int64, budget int) *httptest.Server {
	t.Helper()
	keyLen := (priv.N.BitLen() + 7) / 8
	var mu sync.Mutex
	remaining := budget
	spent := map[string]bool{}
	pubDER, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))

	signAll := func(blinded []string) ([]string, bool) {
		out := make([]string, len(blinded))
		for i, bl := range blinded {
			raw, err := base64.StdEncoding.DecodeString(bl)
			if err != nil || len(raw) != keyLen {
				return nil, false
			}
			s, err := blindrsa.BlindSign(priv, raw)
			if err != nil {
				return nil, false
			}
			out[i] = base64.StdEncoding.EncodeToString(s)
		}
		return out, true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/issue/pubkey", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"pubkey_pem": pubPEM, "denom": "1GBmo"})
	})
	mux.HandleFunc("/sub/voucher", func(w http.ResponseWriter, r *http.Request) {
		var req subVoucherReq
		json.NewDecoder(r.Body).Decode(&req)
		h := sha256.Sum256([]byte(req.SubKey))
		sh := base64.RawURLEncoding.EncodeToString(h[:])
		mu.Lock()
		defer mu.Unlock()
		if sh != subHash || req.Epoch != epoch || len(req.Blinded) > remaining {
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		sigs, ok := signAll(req.Blinded)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		remaining -= len(req.Blinded)
		json.NewEncoder(w).Encode(map[string][]string{"blind_sigs": sigs})
	})
	mux.HandleFunc("/refill", func(w http.ResponseWriter, r *http.Request) {
		var req refillReq
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Vouchers) != len(req.Blinded) || len(req.Vouchers) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		keys := make([]string, len(req.Vouchers))
		for i, v := range req.Vouchers {
			msg, e1 := base64.StdEncoding.DecodeString(v.Msg)
			sig, e2 := base64.StdEncoding.DecodeString(v.Sig)
			if e1 != nil || e2 != nil || blindrsa.Verify(&priv.PublicKey, msg, sig) != nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			p := strings.SplitN(string(msg), "|", 3) // voucher|epoch|serial
			if len(p) != 3 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			k := p[1] + "|" + p[2]
			if spent[k] {
				w.WriteHeader(http.StatusConflict)
				return
			}
			keys[i] = k
		}
		for _, k := range keys {
			spent[k] = true
		}
		sigs, ok := signAll(req.Blinded)
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string][]string{"blind_sigs": sigs})
	})
	return httptest.NewServer(mux)
}

// subscribe stores the subkey and drains the epoch's whole budget into spendable tokens (without knowing the
// count), unlinkably; a second refill in the same epoch is a no-op (budget spent), proving idempotency.
func TestSubscribeAndRefill(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	subkey := "test-subscription-secret"
	h := sha256.Sum256([]byte(subkey))
	subHash := base64.RawURLEncoding.EncodeToString(h[:])
	epoch := currentSubEpoch()

	srv := mockSubGateway(t, priv, subHash, epoch, 5) // this epoch is paid for 5 vouchers
	defer srv.Close()
	g := newGatewayClient(srv.URL)
	wp := filepath.Join(t.TempDir(), "wallet.json")

	n, err := subscribe(g, wp, subkey)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if n != 5 {
		t.Fatalf("subscribe added %d tokens, want 5", n)
	}
	w, _ := loadWallet(wp)
	if len(w.BlindTokens) != 5 {
		t.Fatalf("wallet has %d blind tokens, want 5", len(w.BlindTokens))
	}
	if w.SubKey != subkey || w.SubEpoch != epoch {
		t.Fatalf("subscription state not persisted: key=%q epoch=%d (want epoch %d)", w.SubKey, w.SubEpoch, epoch)
	}
	if len(w.Vouchers) != 0 {
		t.Fatalf("leftover vouchers: %d (refill should clear them)", len(w.Vouchers))
	}

	// Idempotent within the epoch: the budget is spent, so a re-refill adds nothing and doesn't error.
	n2, err := refillNow(g, wp)
	if err != nil {
		t.Fatalf("second refill: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second refill added %d tokens, want 0 (epoch budget already drained)", n2)
	}
	// maybeRefill is also a no-op now (same epoch, no leftovers).
	maybeRefill(g, wp)
	w, _ = loadWallet(wp)
	if len(w.BlindTokens) != 5 {
		t.Fatalf("maybeRefill changed the wallet: %d tokens", len(w.BlindTokens))
	}
}

// A wallet with no subkey never refills.
func TestRefillNoSubkeyNoop(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := mockSubGateway(t, priv, "x", currentSubEpoch(), 5)
	defer srv.Close()
	wp := filepath.Join(t.TempDir(), "wallet.json")
	n, err := refillNow(newGatewayClient(srv.URL), wp)
	if err != nil || n != 0 {
		t.Fatalf("no-subkey refill: n=%d err=%v, want 0/nil", n, err)
	}
}
