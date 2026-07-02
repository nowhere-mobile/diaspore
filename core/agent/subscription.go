package main

// Subscriptions P3 (device half; design: nowhere-cloud/docs/subscription-blind-voucher.md). The device holds
// a bearer SUBSCRIPTION SECRET (subkey) and, once per billing epoch, refills its wallet UNLINKABLY:
//   1. pull epoch vouchers   -- POST /sub/voucher (authed by the subkey), blind-signed so the gateway can't
//      see them; drain the epoch's paid budget without knowing the count (double-on-success / halve-on-402,
//      exactly like drainClaim).
//   2. redeem them for tokens -- POST /refill (anonymous): present the unblinded vouchers, get one spendable
//      blind GB-month token each.
// Vouchers are persisted between the two hops so an interrupted refill resumes instead of losing paid budget.
// The refill is unlinkable to the subscription (both hops are blind), matching the prepaid privacy property.

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"nowhereagent/blindrsa"
)

// subEpochSeconds MUST match the gateway's epochSeconds and the storefront's subEpochSeconds (one billing
// period). DISTINCT from billingEpochSeconds (the 7-day lease epoch) -- the subscription epoch is 30 days, so
// the device credits the same epoch the storefront recorded.
const subEpochSeconds = 2592000

func currentSubEpoch() int64 { return time.Now().Unix() / subEpochSeconds }

// voucher is a blind-signed epoch refill ticket: "voucher|<epoch>|<serial>" + the unblinded signature, both
// base64 (std). It rides in the wallet between the /sub/voucher and /refill hops.
type voucher struct {
	Msg string `json:"msg"`
	Sig string `json:"sig"`
}

type subVoucherReq struct {
	SubKey  string   `json:"sub_key"`
	Epoch   int64    `json:"epoch"`
	Blinded []string `json:"blinded"`
}

func (g *gatewayClient) subVoucher(subkey string, epoch int64, blinded []string) ([]string, int, error) {
	var out issueResp
	code, err := g.postJSON("/sub/voucher", subVoucherReq{SubKey: subkey, Epoch: epoch, Blinded: blinded}, &out)
	return out.BlindSigs, code, err
}

type refillReq struct {
	Vouchers []voucher `json:"vouchers"`
	Blinded  []string  `json:"blinded"`
}

func (g *gatewayClient) refill(vs []voucher, blinded []string) ([]string, int, error) {
	var out issueResp
	code, err := g.postJSON("/refill", refillReq{Vouchers: vs, Blinded: blinded}, &out)
	return out.BlindSigs, code, err
}

// voucherBatch blinds n epoch vouchers, POSTs /sub/voucher, and Finalizes each signature. Mirrors issueBatch;
// returns the HTTP status so a caller can tell a 402 (budget exhausted) apart from a transport/crypto error.
func (g *gatewayClient) voucherBatch(pub *rsa.PublicKey, subkey string, epoch int64, n int) ([]voucher, int, error) {
	msgs := make([][]byte, n)
	invs := make([][]byte, n)
	blinded := make([]string, n)
	for i := 0; i < n; i++ {
		var nb [12]byte
		if _, err := rand.Read(nb[:]); err != nil {
			return nil, 0, err
		}
		msg := []byte("voucher|" + strconv.FormatInt(epoch, 10) + "|" + base64.RawURLEncoding.EncodeToString(nb[:]))
		bl, inv, err := blindrsa.Blind(pub, msg)
		if err != nil {
			return nil, 0, err
		}
		msgs[i], invs[i], blinded[i] = msg, inv, base64.StdEncoding.EncodeToString(bl)
	}
	sigs, code, err := g.subVoucher(subkey, epoch, blinded)
	if err != nil {
		return nil, code, err
	}
	if code != http.StatusOK {
		return nil, code, nil // e.g. 402 (subscription not paid this epoch / budget exhausted) -- caller decides
	}
	if len(sigs) != n {
		return nil, code, fmt.Errorf("sub/voucher: got %d signatures, want %d", len(sigs), n)
	}
	vs := make([]voucher, n)
	for i, s := range sigs {
		bs, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, code, fmt.Errorf("sub/voucher: bad blind signature encoding: %w", err)
		}
		sig, err := blindrsa.Finalize(pub, msgs[i], bs, invs[i]) // unblinds AND verifies; fails closed
		if err != nil {
			return nil, code, fmt.Errorf("sub/voucher: finalize/verify failed: %w", err)
		}
		vs[i] = voucher{Msg: base64.StdEncoding.EncodeToString(msgs[i]), Sig: base64.StdEncoding.EncodeToString(sig)}
	}
	return vs, code, nil
}

// refillBatch turns held vouchers into spendable GB-month tokens: blind one token per voucher, POST /refill,
// Finalize each. /refill is atomic (all serials burn or none), so this batch wholly succeeds or wholly fails.
func (g *gatewayClient) refillBatch(pub *rsa.PublicKey, denom string, vs []voucher) ([]BlindToken, int, error) {
	n := len(vs)
	exp := time.Now().Add(blindTokenTTL).Unix()
	msgs := make([][]byte, n)
	invs := make([][]byte, n)
	blinded := make([]string, n)
	for i := 0; i < n; i++ {
		var nb [12]byte
		if _, err := rand.Read(nb[:]); err != nil {
			return nil, 0, err
		}
		msg := []byte(denom + "|" + base64.RawURLEncoding.EncodeToString(nb[:]) + "|" + strconv.FormatInt(exp, 10))
		bl, inv, err := blindrsa.Blind(pub, msg)
		if err != nil {
			return nil, 0, err
		}
		msgs[i], invs[i], blinded[i] = msg, inv, base64.StdEncoding.EncodeToString(bl)
	}
	sigs, code, err := g.refill(vs, blinded)
	if err != nil {
		return nil, code, err
	}
	if code != http.StatusOK {
		return nil, code, nil
	}
	if len(sigs) != n {
		return nil, code, fmt.Errorf("refill: got %d signatures, want %d", len(sigs), n)
	}
	toks := make([]BlindToken, n)
	for i, s := range sigs {
		bs, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, code, fmt.Errorf("refill: bad blind signature encoding: %w", err)
		}
		sig, err := blindrsa.Finalize(pub, msgs[i], bs, invs[i])
		if err != nil {
			return nil, code, fmt.Errorf("refill: finalize/verify failed: %w", err)
		}
		toks[i] = BlindToken{Msg: base64.StdEncoding.EncodeToString(msgs[i]), Sig: base64.StdEncoding.EncodeToString(sig)}
	}
	return toks, code, nil
}

// appendVouchers persists pulled-but-not-yet-redeemed vouchers (atomic temp+rename) so an interrupted refill
// can resume without re-spending the subscription's epoch budget.
func appendVouchers(walletPath string, vs []voucher) error {
	w, err := loadWallet(walletPath)
	if err != nil {
		return err
	}
	w.Vouchers = append(w.Vouchers, vs...)
	return saveWallet(walletPath, w)
}

// redeemHeldVouchers drains wallet.Vouchers into spendable tokens via /refill, in <=512 chunks, persisting
// each success (remove the chunk, add the tokens) so a crash leaves the wallet consistent. Returns tokens
// added. Used both for leftovers from an interrupted run and for freshly-pulled vouchers.
func redeemHeldVouchers(g *gatewayClient, walletPath string, pub *rsa.PublicKey, denom string) (int, error) {
	added := 0
	for {
		w, err := loadWallet(walletPath)
		if err != nil {
			return added, err
		}
		if len(w.Vouchers) == 0 {
			return added, nil
		}
		chunk := w.Vouchers
		if len(chunk) > 512 {
			chunk = chunk[:512]
		}
		toks, code, err := g.refillBatch(pub, denom, chunk)
		if err != nil {
			return added, err
		}
		if code != http.StatusOK {
			return added, fmt.Errorf("refill: HTTP %d", code)
		}
		w.Vouchers = append([]voucher(nil), w.Vouchers[len(chunk):]...)
		w.BlindTokens = append(w.BlindTokens, toks...)
		if err := saveWallet(walletPath, w); err != nil {
			return added, err
		}
		added += len(toks)
	}
}

// refillNow tops the wallet up for the CURRENT epoch from the subscription: redeem any leftover vouchers,
// drain this epoch's voucher budget (double/halve like drainClaim), redeeming each batch to tokens as it
// lands, then mark the epoch done. No-op without a subkey. Safe to call repeatedly (idempotent per epoch via
// the gateway's one-credit-per-(subHash,epoch) budget + per-epoch serial burn).
func refillNow(g *gatewayClient, walletPath string) (int, error) {
	w, err := loadWallet(walletPath)
	if err != nil {
		return 0, err
	}
	if w.SubKey == "" {
		return 0, nil
	}
	pub, denom, err := g.issuePubKey()
	if err != nil {
		return 0, err
	}
	added := 0
	if n, err := redeemHeldVouchers(g, walletPath, pub, denom); err != nil { // finish an interrupted run first
		return added, err
	} else {
		added += n
	}
	epoch := currentSubEpoch()
	batch := 1
	for {
		vs, code, err := g.voucherBatch(pub, w.SubKey, epoch, batch)
		if err != nil {
			if added > 0 {
				return added, nil // keep what landed
			}
			return added, err
		}
		if code == http.StatusPaymentRequired {
			if batch == 1 {
				break // epoch budget exhausted -> done
			}
			batch /= 2 // overshot; drain the tail
			continue
		}
		if code != http.StatusOK {
			if added > 0 {
				return added, nil
			}
			return added, fmt.Errorf("sub/voucher: HTTP %d", code)
		}
		if err := appendVouchers(walletPath, vs); err != nil { // persist BEFORE redeeming so a crash resumes
			return added, err
		}
		n, err := redeemHeldVouchers(g, walletPath, pub, denom)
		if err != nil {
			return added, err // the vouchers stay persisted for the next refillNow
		}
		added += n
		if batch < 256 {
			batch *= 2
		}
	}
	w2, err := loadWallet(walletPath)
	if err != nil {
		return added, err
	}
	w2.SubEpoch = epoch
	if err := saveWallet(walletPath, w2); err != nil {
		return added, err
	}
	return added, nil
}

// subscribe stores the bearer subkey and does an immediate first refill. The subkey roams with the wallet, so
// every device the profile logs into auto-refills.
func subscribe(g *gatewayClient, walletPath, subkey string) (int, error) {
	if subkey == "" {
		return 0, errors.New("subscribe: empty subscription key")
	}
	w, err := loadWallet(walletPath)
	if err != nil {
		return 0, err
	}
	w.SubKey = subkey
	w.SubEpoch = 0 // force a refill this epoch
	if err := saveWallet(walletPath, w); err != nil {
		return 0, err
	}
	return refillNow(g, walletPath)
}

// maybeRefill auto-tops-up the live session once per epoch -- called from the billing maintenance alongside
// payRent. Best-effort + double-gated: only when a subkey is set AND this epoch isn't already refilled (or a
// leftover voucher is pending). Never blocks or fails the caller.
func maybeRefill(g *gatewayClient, walletPath string) {
	defer func() { recover() }()
	w, err := loadWallet(walletPath)
	if err != nil || w.SubKey == "" {
		return
	}
	if w.SubEpoch >= currentSubEpoch() && len(w.Vouchers) == 0 {
		return // already refilled this epoch, nothing pending
	}
	if n, err := refillNow(g, walletPath); err != nil {
		fmt.Fprintf(os.Stderr, "[logind] sub-refill: %v\n", err)
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "[logind] sub-refill: +%d token(s) for epoch %d\n", n, currentSubEpoch())
	}
}
