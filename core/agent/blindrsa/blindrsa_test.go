package blindrsa

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"testing"
)

// The full client+issuer round-trip: a blinded token, blind-signed without the issuer seeing it, unblinds
// to a signature that Verify accepts.
func TestRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub := &priv.PublicKey
	msg := []byte("1GBmo|abcd1234|1750000000")

	blinded, inv, err := Blind(pub, msg)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := BlindSign(priv, blinded)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := Finalize(pub, msg, bs, inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, msg, sig); err != nil {
		t.Fatalf("verify finalized token: %v", err)
	}
}

// Interop pin: a finalized token must verify as an ordinary stdlib RSA-PSS signature, AND a stdlib SignPSS
// signature must pass our Verify. This anchors the device's params (SHA-384, saltLen=48) to the exact ones
// the gateway uses, so device<->gateway issue/redeem can't silently drift.
func TestInteropWithStdlibPSS(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	pub := &priv.PublicKey
	msg := []byte("1GBmo|deadbeef|1760000000")

	blinded, inv, err := Blind(pub, msg)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := BlindSign(priv, blinded)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := Finalize(pub, msg, bs, inv)
	if err != nil {
		t.Fatal(err)
	}
	h := newHash()
	h.Write(msg)
	if err := rsa.VerifyPSS(pub, crypto.SHA384, h.Sum(nil), sig, &rsa.PSSOptions{SaltLength: saltLen, Hash: crypto.SHA384}); err != nil {
		t.Fatalf("finalized sig is not a valid stdlib PSS signature: %v", err)
	}

	h.Reset()
	h.Write(msg)
	std, err := rsa.SignPSS(rand.Reader, priv, crypto.SHA384, h.Sum(nil), &rsa.PSSOptions{SaltLength: saltLen, Hash: crypto.SHA384})
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(pub, msg, std); err != nil {
		t.Fatalf("stdlib PSS signature rejected by Verify: %v", err)
	}
}

func TestTamperedMessageFails(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	pub := &priv.PublicKey
	msg := []byte("1GBmo|aaaa|1")
	blinded, inv, _ := Blind(pub, msg)
	bs, _ := BlindSign(priv, blinded)
	sig, _ := Finalize(pub, msg, bs, inv)
	if err := Verify(pub, []byte("1GBmo|aaaa|2"), sig); err == nil {
		t.Fatal("a tampered message must not verify")
	}
}
