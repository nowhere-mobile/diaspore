package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A fresh device with only DISCO_ENDPOINT + DISCO_BUCKET (no creds) bootstraps its store config from a
// public-read discovery bucket over an anonymous HTTP GET -- proving the keyless lookup path end to end
// (URL build + GET + unseal). Discovery only holds sealed blobs at unguessable refs, so public-read is safe.
func TestDiscoAnonLookup(t *testing.T) {
	name, pass := "alice", "correct horse battery"
	cfg := "S3_ENDPOINT=https://s3.filebase.io\nS3_REGION=auto\nS3_BUCKET=data\nS3_ACCESS_KEY=AK\nS3_SECRET_KEY=SK\n"
	sealed := wrap(bootstrapKey(name, pass), []byte(cfg)) // exactly what publishDiscovery would PUT
	bucket := "disco-bucket"
	wantPath := "/" + bucket + "/ref/" + bootstrapRef(name, pass)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == wantPath {
			io.WriteString(w, sealed)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("DISCO_ENDPOINT", srv.URL)
	t.Setenv("DISCO_BUCKET", bucket)
	t.Setenv("DISCO_ACCESS_KEY", "") // no creds -> anonymous path
	t.Setenv("DISCO_SECRET_KEY", "")

	if !discoCanLookup() {
		t.Fatal("endpoint+bucket should be enough to look up")
	}
	if discoConfigured() {
		t.Fatal("with no keys the device must NOT be publish-configured")
	}

	got, ok := discoverConfig("disco", name, pass)
	if !ok {
		t.Fatal("anonymous discovery lookup should succeed against a public-read bucket")
	}
	if got != cfg {
		t.Fatalf("discovered config mismatch:\n got %q\nwant %q", got, cfg)
	}

	// Blind: a wrong passphrase derives a different bootstrapRef -> a GET miss, indistinguishable.
	if _, ok := discoverConfig("disco", name, "wrong-pass"); ok {
		t.Fatal("a wrong passphrase must miss (blind login)")
	}
}

// TestGatewayRoamsViaDiscovery (DIA-20260627-B / B.4): the billing GATEWAY_URL rides the sealed store-config
// so a user's gateway re-materializes on another device from name+pass alone; an empty gateway is omitted.
func TestGatewayRoamsViaDiscovery(t *testing.T) {
	base := newHTTPStore(t)
	name, pass := "gw", "roam my gateway too"

	publishDiscovery(base, name, pass,
		"https://s3.test", "auto", "bkt", "AK", "SK", "https://gw.nowhere.mobile")
	cfg, ok := discoverConfig(base, name, pass)
	if !ok {
		t.Fatal("discover after publish should succeed")
	}
	if !strings.Contains(cfg, "GATEWAY_URL=https://gw.nowhere.mobile") {
		t.Fatalf("gateway URL did not roam with the store config:\n%s", cfg)
	}

	// an empty gateway is omitted: older readers see only the S3_* lines.
	publishDiscovery(base, name, pass, "https://s3.test", "auto", "bkt", "AK", "SK", "")
	cfg2, _ := discoverConfig(base, name, pass)
	if strings.Contains(cfg2, "GATEWAY_URL") {
		t.Fatalf("an empty gateway should be omitted from the roamed config:\n%s", cfg2)
	}
}
