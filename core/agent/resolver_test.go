package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// the real shape of a `dumpsys connectivity` DnsAddresses list (IPv6 + the network's own IPv4 resolvers)
const sampleDumpsys = `NetworkAgentInfo{network{104} lp{{InterfaceName: wlan0 ` +
	`DnsAddresses: [ /2001:4860:4860::8844,/2001:4860:4860::8888,/71.250.0.12,/68.237.161.12 ] ServerAddress: /192.168.4.1}}`

func TestPickDNS(t *testing.T) {
	cases := []struct {
		name     string
		override string
		props    []string
		dumpsys  string
		want     []string
	}{
		{"explicit override wins", "8.8.8.8, 8.8.4.4", []string{"192.168.1.1"}, sampleDumpsys, []string{"8.8.8.8", "8.8.4.4"}},
		{"props when no override", "", []string{"192.168.1.1", "10.0.0.1"}, sampleDumpsys, []string{"192.168.1.1", "10.0.0.1"}},
		{"dumpsys when override+props empty (IPv4 first)", "", nil, sampleDumpsys,
			[]string{"71.250.0.12", "68.237.161.12", "2001:4860:4860::8844", "2001:4860:4860::8888"}},
		{"public fallback when all empty", "", nil, "", []string{"1.1.1.1"}},
		{"garbage ignored, dedup", "1.2.3.4, notanip, 1.2.3.4", nil, "", []string{"1.2.3.4"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickDNS(c.override, c.props, c.dumpsys); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("pickDNS = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseDumpsysDNS(t *testing.T) {
	got := parseDumpsysDNS(sampleDumpsys)
	want := []string{"/2001:4860:4860::8844", "/2001:4860:4860::8888", "/71.250.0.12", "/68.237.161.12"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseDumpsysDNS = %v, want %v", got, want)
	}
	if parseDumpsysDNS("no dns here") != nil {
		t.Fatal("expected nil when DnsAddresses is absent")
	}
}

func TestHasResolvConf(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	_ = os.WriteFile(good, []byte("# comment\nnameserver 1.1.1.1\n"), 0o644)
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("# nothing useful\n"), 0o644)

	if !hasResolvConf(good) {
		t.Error("file with a nameserver should be usable")
	}
	if hasResolvConf(empty) {
		t.Error("file with no nameserver should be unusable")
	}
	if hasResolvConf(filepath.Join(dir, "missing")) {
		t.Error("missing file should be unusable")
	}
}

func TestWithPort53(t *testing.T) {
	for in, want := range map[string]string{
		"1.1.1.1":        "1.1.1.1:53",
		"1.1.1.1:5353":   "1.1.1.1:5353",
		"2001:4860::8888": "[2001:4860::8888]:53",
	} {
		if got := withPort53(in); got != want {
			t.Errorf("withPort53(%q) = %q, want %q", in, got, want)
		}
	}
}
