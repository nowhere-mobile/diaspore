// Package blindrsa implements RSA blind signatures (RSA-PSS, SHA-384 / MGF1-SHA-384) for nowhere's
// zero-knowledge storage tokens: the device blinds a token locally, the gateway signs it WITHOUT seeing
// it, the device unblinds, and the gateway later verifies + double-spend-checks the unblinded token — so
// the gateway can never link an issued token to a redeemed one. RFC 9474 family.
//
// This is the DEVICE (client) half of nowhere-cloud's internal/blindrsa, kept byte-identical so the
// parameters can never drift: SHA-384 / MGF1-SHA-384, saltLen=48, emBits=N.BitLen()-1. Any change here
// MUST be mirrored in nowhere-cloud/internal/blindrsa or interop (issue/redeem) breaks. The device only
// needs Blind/Finalize/Verify; BlindSign is retained so the package's round-trip test can stand alone.
//
// Stdlib only (no third-party deps), per AGENTS.md. Correctness is anchored by stdlib
// crypto/rsa.VerifyPSS: a finalized signature MUST verify as an ordinary RSA-PSS signature, so any bug in
// the hand-rolled EMSA-PSS encoding or the blinding fails the round-trip test loudly.
//
// Flow:
//
//	device:  blinded, inv := Blind(pub, token)         // PSS-encode + blind
//	gateway: blindSig     := BlindSign(priv, blinded)  // raw RSA, never sees the token
//	device:  sig          := Finalize(pub, token, blindSig, inv)  // unblind + verify
//	gateway: Verify(pub, token, sig)                   // at redeem; + a spent-set for double-spend
package blindrsa

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha512"
	"errors"
	"hash"
	"io"
	"math/big"
)

// saltLen is the PSS salt length (SHA-384 output size). We encode with it and verify against it.
const saltLen = 48

func newHash() hash.Hash { return sha512.New384() }

// Blind PSS-encodes msg and blinds it for the issuer. It returns the blinded message to send to the
// issuer and the inverse the caller keeps to unblind the result. Uses crypto/rand.
func Blind(pub *rsa.PublicKey, msg []byte) (blindedMsg, blindInv []byte, err error) {
	return blindWithRand(rand.Reader, pub, msg)
}

func blindWithRand(rnd io.Reader, pub *rsa.PublicKey, msg []byte) (blindedMsg, blindInv []byte, err error) {
	if pub.E <= 0 {
		return nil, nil, errors.New("blindrsa: bad public exponent")
	}
	k := (pub.N.BitLen() + 7) / 8

	h := newHash()
	h.Write(msg)
	mHash := h.Sum(nil)

	salt := make([]byte, saltLen)
	if _, err = io.ReadFull(rnd, salt); err != nil {
		return nil, nil, err
	}
	em, err := emsaPSSEncode(mHash, salt, pub.N.BitLen()-1)
	if err != nil {
		return nil, nil, err
	}
	m := new(big.Int).SetBytes(em)
	if m.Cmp(pub.N) >= 0 {
		return nil, nil, errors.New("blindrsa: encoded message >= modulus")
	}

	// random blinding factor r in (0, n) coprime to n, with inverse rInv mod n
	var r, rInv *big.Int
	for {
		r, err = rand.Int(rnd, pub.N)
		if err != nil {
			return nil, nil, err
		}
		if r.Sign() == 0 {
			continue
		}
		if rInv = new(big.Int).ModInverse(r, pub.N); rInv != nil {
			break
		}
	}
	e := big.NewInt(int64(pub.E))
	x := new(big.Int).Exp(r, e, pub.N)                   // r^e mod n
	z := new(big.Int).Mod(new(big.Int).Mul(m, x), pub.N) // m * r^e mod n
	return i2osp(z, k), i2osp(rInv, k), nil
}

// BlindSign is the issuer half: a raw RSA exponentiation over the blinded message (z^d mod n). The issuer
// never sees the token, only the opaque blinded value.
func BlindSign(priv *rsa.PrivateKey, blindedMsg []byte) ([]byte, error) {
	k := (priv.N.BitLen() + 7) / 8
	if len(blindedMsg) != k {
		return nil, errors.New("blindrsa: blinded message wrong length")
	}
	z := new(big.Int).SetBytes(blindedMsg)
	if z.Cmp(priv.N) >= 0 {
		return nil, errors.New("blindrsa: blinded message >= modulus")
	}
	s := new(big.Int).Exp(z, priv.D, priv.N) // z^d mod n
	// Round-trip check (s^e == z): guards against a malformed key / fault attacks before we emit a sig.
	if new(big.Int).Exp(s, big.NewInt(int64(priv.E)), priv.N).Cmp(z) != 0 {
		return nil, errors.New("blindrsa: signing round-trip check failed")
	}
	return i2osp(s, k), nil
}

// Finalize unblinds the issuer's blind signature into a normal RSA-PSS signature over msg, verifying it
// before returning so a bad issuer response fails closed.
func Finalize(pub *rsa.PublicKey, msg, blindSig, blindInv []byte) ([]byte, error) {
	k := (pub.N.BitLen() + 7) / 8
	if len(blindSig) != k || len(blindInv) != k {
		return nil, errors.New("blindrsa: blindSig/blindInv wrong length")
	}
	s := new(big.Int).SetBytes(blindSig)
	rInv := new(big.Int).SetBytes(blindInv)
	sig := new(big.Int).Mod(new(big.Int).Mul(s, rInv), pub.N) // s * rInv mod n
	sigBytes := i2osp(sig, k)
	if err := Verify(pub, msg, sigBytes); err != nil {
		return nil, err
	}
	return sigBytes, nil
}

// Verify checks a finalized token as an ordinary RSA-PSS signature — the ground truth for the whole scheme.
func Verify(pub *rsa.PublicKey, msg, sig []byte) error {
	h := newHash()
	h.Write(msg)
	return rsa.VerifyPSS(pub, crypto.SHA384, h.Sum(nil), sig, &rsa.PSSOptions{SaltLength: saltLen, Hash: crypto.SHA384})
}

// --- EMSA-PSS-ENCODE (RFC 8017 §9.1.1) + MGF1: the piece crypto/rsa doesn't export ---

func emsaPSSEncode(mHash, salt []byte, emBits int) ([]byte, error) {
	hLen := len(mHash)
	sLen := len(salt)
	emLen := (emBits + 7) / 8
	if emLen < hLen+sLen+2 {
		return nil, errors.New("blindrsa: encoding error (emLen too small)")
	}
	// H = Hash(0x00*8 || mHash || salt)
	h := newHash()
	h.Write(make([]byte, 8))
	h.Write(mHash)
	h.Write(salt)
	hh := h.Sum(nil)
	// DB = PS(0x00 * (emLen-sLen-hLen-2)) || 0x01 || salt
	db := make([]byte, emLen-hLen-1)
	db[emLen-sLen-hLen-2] = 0x01
	copy(db[emLen-sLen-hLen-1:], salt)
	dbMask := mgf1(hh, emLen-hLen-1)
	for i := range db {
		db[i] ^= dbMask[i]
	}
	db[0] &= 0xff >> (8*emLen - emBits) // clear the leftmost 8*emLen-emBits bits so OS2IP(EM) < 2^emBits
	em := make([]byte, emLen)
	copy(em, db)
	copy(em[emLen-hLen-1:], hh)
	em[emLen-1] = 0xbc
	return em, nil
}

func mgf1(seed []byte, length int) []byte {
	h := newHash()
	var out []byte
	var counter [4]byte
	for i := 0; len(out) < length; i++ {
		counter[0], counter[1], counter[2], counter[3] = byte(i>>24), byte(i>>16), byte(i>>8), byte(i)
		h.Reset()
		h.Write(seed)
		h.Write(counter[:])
		out = h.Sum(out)
	}
	return out[:length]
}

func i2osp(x *big.Int, length int) []byte {
	out := make([]byte, length)
	x.FillBytes(out) // big-endian, left-zero-padded to length; panics only if x doesn't fit (guarded above)
	return out
}
