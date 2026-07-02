package main

// On-device DNS for the agent. The agent is built CGO_ENABLED=0, so Go's pure resolver reads
// /etc/resolv.conf -- which Android does not provide (and the net.dns* props are empty) -- and otherwise
// falls back to localhost:53, where nothing listens, so every hostname lookup fails ("dial ... [::1]:53:
// connection refused"). DNS on Android lives in netd, which a cgo-less Go binary can't reach. We instead
// point net.DefaultResolver at a real nameserver, PREFERRING the device's own per-network DNS (discovered
// from the connectivity service, so queries don't leave the network the device is already on) over a
// public fallback. This supersedes the previous fixed NOWHERE_DNS=1.1.1.1:53 default the launcher scripts
// set, which sent every lookup to Cloudflare.

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// installResolver overrides the process resolver when the platform has no usable resolv.conf. Precedence:
// an explicit NOWHERE_DNS operator override > a working /etc/resolv.conf (a normal Linux host / the build
// VM: no-op, so tests and native tools are unaffected) > the device's discovered per-network DNS > a
// public fallback. Call once, early in main(), before any network use.
func installResolver() {
	if dns := strings.TrimSpace(os.Getenv("NOWHERE_DNS")); dns != "" {
		setResolver([]string{withPort53(dns)}) // explicit operator override (host or host:port)
		return
	}
	if hasResolvConf("/etc/resolv.conf") {
		return // normal host: Go's resolver already works -- leave it alone
	}
	servers := pickDNS("", getpropMulti("net.dns1", "net.dns2", "dhcp.wlan0.dns1", "dhcp.wlan0.dns2"), dumpsysConnectivity())
	if len(servers) == 0 {
		return // nothing to point at; don't make it worse
	}
	addrs := make([]string, len(servers))
	for i, s := range servers {
		addrs[i] = net.JoinHostPort(s, "53")
	}
	setResolver(addrs)
}

// setResolver routes all DNS through the given server addresses (host:port), tried in order until one
// connects. PreferGo forces the pure resolver (cgo is off anyway); the network arg (udp/tcp) is honored --
// only the server address is overridden.
func setResolver(addrs []string) {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			var err error
			for _, a := range addrs {
				var c net.Conn
				if c, err = d.DialContext(ctx, network, a); err == nil {
					return c, nil
				}
			}
			if err == nil {
				err = errors.New("resolver: no dns servers configured")
			}
			return nil, err
		},
	}
}

// withPort53 ensures an address has a port, defaulting to 53.
func withPort53(s string) string {
	if _, _, err := net.SplitHostPort(s); err == nil {
		return s
	}
	return net.JoinHostPort(s, "53")
}

// hasResolvConf reports whether path declares at least one nameserver (an absent/empty file means Go's
// resolver has nothing to resolve with and would fall back to localhost).
func hasResolvConf(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(strings.TrimSpace(sc.Text()), "nameserver ") {
			return true
		}
	}
	return false
}

// pickDNS chooses nameserver IPs (IPv4 first, de-duped) from, in priority order: an explicit override,
// then Android system properties, then the connectivity service's per-network DnsAddresses. A public
// resolver is the last resort so the agent still works if discovery yields nothing. Pure (no I/O), so the
// selection logic is unit-testable; the real getprop/dumpsys reads are injected by installResolver.
func pickDNS(override string, props []string, dumpsysOut string) []string {
	var v4, v6 []string
	add := func(tok string) {
		tok = strings.TrimPrefix(strings.TrimSpace(tok), "/") // dumpsys prints addrs as "/1.2.3.4"
		ip := net.ParseIP(tok)
		if ip == nil {
			return
		}
		if ip.To4() != nil {
			v4 = append(v4, tok)
		} else {
			v6 = append(v6, tok)
		}
	}
	addAll := func(s string) {
		for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
			add(t)
		}
	}
	addAll(override)
	if len(v4)+len(v6) == 0 {
		for _, p := range props {
			addAll(p)
		}
	}
	if len(v4)+len(v6) == 0 {
		for _, t := range parseDumpsysDNS(dumpsysOut) {
			add(t)
		}
	}
	out := dedupeStr(append(v4, v6...)) // IPv4 first (plain :53 is most universally reachable)
	if len(out) == 0 {
		out = []string{"1.1.1.1"} // last-resort public resolver; discovery above avoids this on a normal network
	}
	return out
}

// parseDumpsysDNS extracts the first network's DnsAddresses from `dumpsys connectivity`, formatted as:
//
//	DnsAddresses: [ /2001:4860:4860::8888,/71.250.0.12 ]
func parseDumpsysDNS(s string) []string {
	i := strings.Index(s, "DnsAddresses:")
	if i < 0 {
		return nil
	}
	rest := s[i+len("DnsAddresses:"):]
	l := strings.IndexByte(rest, '[')
	r := strings.IndexByte(rest, ']')
	if l < 0 || r < 0 || r <= l {
		return nil
	}
	return strings.FieldsFunc(rest[l+1:r], func(c rune) bool { return c == ',' || c == ' ' || c == '\t' })
}

func dedupeStr(in []string) []string {
	seen := map[string]bool{}
	out := in[:0:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// getpropMulti reads several Android system properties, dropping empty/missing ones.
func getpropMulti(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if v := getprop(n); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func getprop(name string) string {
	b, err := exec.Command("getprop", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func dumpsysConnectivity() string {
	b, err := exec.Command("dumpsys", "connectivity").Output()
	if err != nil {
		return ""
	}
	return string(b)
}
