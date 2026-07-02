package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"nowhereagent/blindrsa"
)

// blindGateway is a minimal stand-in for the real gateway's zero-knowledge endpoints (/quote,
// /issue/pubkey, /issue, /lease) -- enough to exercise the device wallet's claim+redeem flow end to end.
// It blind-signs blinded messages for a known paid claim, and at lease verifies the UNBLINDED tokens with
// the same key + the denom|nonce|exp parse + a nonce spent-set, exactly as internal/gateway does. This is
// the device's interop pin: if the ported blindrsa params drifted from nowhere-cloud, Verify here fails.
func blindGateway(t *testing.T, priv *rsa.PrivateKey, claim string, claimCount int) *httptest.Server {
	t.Helper()
	keyLen := (priv.N.BitLen() + 7) / 8
	spent := map[string]bool{}
	remaining := claimCount
	mux := http.NewServeMux()

	mux.HandleFunc("/quote", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(quoteInfo{Denom: "1GBmo", GBPerToken: 1, CurrentEpoch: 1})
	})
	mux.HandleFunc("/issue/pubkey", func(w http.ResponseWriter, _ *http.Request) {
		der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
		p := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
		_ = json.NewEncoder(w).Encode(map[string]string{"pubkey_pem": p, "denom": "1GBmo"})
	})
	mux.HandleFunc("/issue", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Claim   string   `json:"claim"`
			Blinded []string `json:"blinded"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Claim != claim || len(req.Blinded) == 0 || len(req.Blinded) > remaining {
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		sigs := make([]string, len(req.Blinded))
		for i, b := range req.Blinded {
			raw, err := base64.StdEncoding.DecodeString(b)
			if err != nil || len(raw) != keyLen {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s, err := blindrsa.BlindSign(priv, raw) // signs WITHOUT seeing the token
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sigs[i] = base64.StdEncoding.EncodeToString(s)
		}
		remaining -= len(req.Blinded)
		_ = json.NewEncoder(w).Encode(map[string][]string{"blind_sigs": sigs})
	})
	mux.HandleFunc("/lease", func(w http.ResponseWriter, r *http.Request) {
		var req leaseReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		var total int64
		for _, sz := range req.Refs {
			if sz > 0 {
				total += sz
			}
		}
		owed := int(math.Ceil(float64(total) / float64(1<<30)))
		if owed == 0 {
			owed = 1
		}
		good := 0
		for _, bt := range req.BlindTokens {
			msg, e1 := base64.StdEncoding.DecodeString(bt.Msg)
			sig, e2 := base64.StdEncoding.DecodeString(bt.Sig)
			if e1 != nil || e2 != nil {
				continue
			}
			if blindrsa.Verify(&priv.PublicKey, msg, sig) != nil {
				continue
			}
			parts := strings.SplitN(string(msg), "|", 3)
			if len(parts) != 3 || parts[0] != "1GBmo" || spent[parts[1]] {
				continue
			}
			exp, err := strconv.ParseInt(parts[2], 10, 64)
			if err != nil || time.Now().Unix() > exp {
				continue
			}
			spent[parts[1]] = true
			good++
			if good == owed {
				break
			}
		}
		if good < owed {
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		_ = json.NewEncoder(w).Encode(leaseInfo{ThroughEpoch: 5, Spent: owed})
	})
	return httptest.NewServer(mux)
}

// The whole device flow: claim a paid code into zero-knowledge tokens, then spend them at a lease. Proves
// the ported blind client interops with the gateway's verify, the wallet stores/spends blind tokens, and
// the spent-set rejects re-spending.
func TestBlindClaimAndLease(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := blindGateway(t, priv, "CLAIM-X", 5)
	defer srv.Close()
	g := newGatewayClient(srv.URL)
	wpath := filepath.Join(t.TempDir(), "wallet.json")

	n, err := claimTokens(g, wpath, "CLAIM-X", 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n != 3 {
		t.Fatalf("claimed %d, want 3", n)
	}
	w, _ := loadWallet(wpath)
	if len(w.BlindTokens) != 3 || w.balance() != 3 {
		t.Fatalf("wallet: %d blind / balance %d, want 3 / 3", len(w.BlindTokens), w.balance())
	}

	// redeem: a 1.5 GiB footprint owes 2 tokens; one zero-knowledge token must remain.
	info, err := leaseRefs(g, wpath, map[string]int64{"blob/a": 1 << 30, "blob/b": 1 << 29})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if info.Spent != 2 {
		t.Fatalf("spent %d, want 2", info.Spent)
	}
	after, _ := loadWallet(wpath)
	if after.balance() != 1 || len(after.BlindTokens) != 1 {
		t.Fatalf("after lease: balance %d / %d blind, want 1 / 1", after.balance(), len(after.BlindTokens))
	}

	// only 1 token left -> a lease owing 2 must report insufficient credit, leaving the wallet untouched.
	if _, err := leaseRefs(g, wpath, map[string]int64{"blob/c": 2 << 30}); err == nil {
		t.Fatal("expected insufficient credit when owed exceeds remaining credit")
	}
	again, _ := loadWallet(wpath)
	if again.balance() != 1 {
		t.Fatalf("wallet drained on a failed lease: balance %d, want 1", again.balance())
	}
}

// claimTokens must persist nothing when the gateway rejects the claim (e.g. over-claim), so a failed
// top-up can't leave half-issued or phantom credit in the wallet.
func TestBlindClaimRejectedLeavesWallet(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := blindGateway(t, priv, "CLAIM-Y", 2)
	defer srv.Close()
	wpath := filepath.Join(t.TempDir(), "wallet.json")

	if _, err := claimTokens(newGatewayClient(srv.URL), wpath, "CLAIM-Y", 3); err == nil {
		t.Fatal("over-claim (3 > 2) should fail")
	}
	w, _ := loadWallet(wpath)
	if w.balance() != 0 {
		t.Fatalf("rejected claim left %d credit, want 0", w.balance())
	}
}

// drainClaim redeems a whole claim code without knowing its count (the "Add credits" path): it converges
// on the exact value via doubling/back-off, never over-claims, and a re-drain or unknown code adds nothing.
func TestDrainClaim(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv := blindGateway(t, priv, "DRAIN-Z", 7)
	defer srv.Close()
	g := newGatewayClient(srv.URL)
	wpath := filepath.Join(t.TempDir(), "wallet.json")

	n, err := drainClaim(g, wpath, "DRAIN-Z")
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 7 {
		t.Fatalf("drained %d, want 7", n)
	}
	if w, _ := loadWallet(wpath); len(w.BlindTokens) != 7 {
		t.Fatalf("wallet has %d zero-knowledge tokens, want 7", len(w.BlindTokens))
	}

	// re-draining a now-spent code adds nothing (and doesn't error)
	if n2, err := drainClaim(g, wpath, "DRAIN-Z"); err != nil || n2 != 0 {
		t.Fatalf("re-drain: n=%d err=%v, want 0 / nil", n2, err)
	}
	// an unknown code drains nothing
	if n3, _ := drainClaim(g, wpath, "NOPE"); n3 != 0 {
		t.Fatalf("unknown code drained %d, want 0", n3)
	}
}
