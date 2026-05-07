package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// ErrSSRFRejected is returned by ValidateCallbackURL (pre-flight) and by
// ssrfGuard.DialContext (dial-time, wrapped via fmt.Errorf %w) when a customer-
// supplied URL targets a blocked address class.
// status_callback_failures_total{reason="ssrf_rejected"} is keyed off this
// sentinel.
var ErrSSRFRejected = errors.New("webhook: callback URL targets blocked address space")

// ssrfGuard wraps a net.Dialer with DNS-resolve-then-validate semantics.
// Defeats DNS rebinding: a multi-IP A-record where
// one entry is public and one is loopback would otherwise let a connect-time
// second DNS lookup return a different IP than the validation lookup saw.
//
// Pattern: resolve once via net.DefaultResolver.LookupIPAddr, validate every
// returned IP, dial the FIRST resolved IP directly (not the hostname).
type ssrfGuard struct {
	dialer *net.Dialer
}

// newSSRFGuard returns a guard with the same dial-side timing as
// webhook.NewClient (Timeout 2s, KeepAlive 30s — see client.go).
func newSSRFGuard() *ssrfGuard {
	return &ssrfGuard{
		dialer: &net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		},
	}
}

// DialContext implements the http.Transport.DialContext signature. Resolve →
// validate-all → dial-IP. RESEARCH §6.2.
func (g *ssrfGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("ssrf: split host port %q: %w", addr, err)
	}
	// Resolve once. Validate every IP. Don't trust subsequent lookups.
	// LookupIPAddr accepts both IP literals (returned unchanged) and hostnames.
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("ssrf: dns lookup %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("ssrf: dns returned no addresses for %q: %w", host, ErrSSRFRejected)
	}
	for _, ip := range ips {
		if isBlockedIP(ip.IP) {
			return nil, fmt.Errorf("ssrf: blocked IP %s for host %s: %w", ip.IP, host, ErrSSRFRejected)
		}
	}
	// Dial the FIRST resolved IP directly so a second DNS lookup at connect
	// time cannot return a different IP. This is the key DNS-rebinding bypass
	// for stdlib's hostname-based net.Dialer.DialContext.
	return g.dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}

// isBlockedIP returns true if ip falls into any address class that an
// outbound webhook MUST NOT target. RESEARCH §6.1 — six stdlib classes plus
// three custom CIDRs (CGNAT, 0/8, broadcast).
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true // defensive — nil cannot be safely dialed
	}
	// I-2 fix: normalize IPv4-mapped IPv6 (::ffff:0:0/96) to plain IPv4
	// BEFORE classification, so attacker URLs of the form
	//   https://[::ffff:7f00:1]/cb         (loopback in mapped form)
	//   https://[::ffff:a9fe:a9fe]/cb      (cloud-metadata in mapped form)
	// do not bypass the IsLoopback / IsLinkLocalUnicast / IsPrivate stdlib
	// helpers (which classify only the IPv4 forms, not the mapped IPv6
	// representation). net.IP.To4 returns nil for non-IPv4-representable
	// addresses, so genuine IPv6 addresses are unaffected.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() {
		return true
	}
	return inBlockedExtraCIDRs(ip)
}

// blockedExtraCIDRs are the ranges not covered by net.IP stdlib helpers.
// Parsed once at package init for table-driven matching.
var blockedExtraCIDRs = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10",      // RFC 6598 CGNAT
		"0.0.0.0/8",          // "this network" (IsUnspecified covers only 0.0.0.0/32)
		"255.255.255.255/32", // limited broadcast
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, ipnet, err := net.ParseCIDR(c)
		if err != nil {
			panic("ssrf: invalid hard-coded CIDR " + c + ": " + err.Error())
		}
		out = append(out, ipnet)
	}
	return out
}()

func inBlockedExtraCIDRs(ip net.IP) bool {
	for _, n := range blockedExtraCIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ValidateCallbackURL is the pre-flight (cheap, no-DNS) rejection path
// called from REST handlers at Enqueue time. RESEARCH §6.3 stage 1.
//
// Returns:
//   - ErrEmptyURL if u == ""
//   - ErrNonHTTPS if scheme != "https"
//   - ErrSSRFRejected if host is the literal string "localhost" OR an IP
//     literal in a blocked range (no DNS lookup at this stage; full DNS
//     rebinding mitigation happens at dial time via ssrfGuard.DialContext)
//   - nil otherwise (the URL passed pre-flight; dial-time may still reject)
func ValidateCallbackURL(u string) error {
	if u == "" {
		return ErrEmptyURL
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("ssrf: parse URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return ErrNonHTTPS
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("ssrf: URL has empty host: %w", ErrSSRFRejected)
	}
	// Literal "localhost" — case-insensitive — without a DNS lookup. (DNS
	// rebinding via "localhost.attacker.example" lands at the dial-time
	// stage; this is the cheap-rejection layer.)
	if strings.EqualFold(host, "localhost") {
		return fmt.Errorf("ssrf: literal localhost host: %w", ErrSSRFRejected)
	}
	// IP literal — short-circuit the DNS path and check the table directly.
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("ssrf: literal IP %s in blocked range: %w", host, ErrSSRFRejected)
		}
	}
	// Hostname — ssrfGuard.DialContext will do the DNS check at connect time.
	return nil
}
