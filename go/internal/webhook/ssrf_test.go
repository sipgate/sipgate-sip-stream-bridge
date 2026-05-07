package webhook

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestIsBlockedIP_TableDriven covers RESEARCH §6.1 CIDR table — every class
// must reject; public IPs (v4 + v6) must NOT reject; CGNAT-edge cases verify
// the boundary; ::ffff:* IPv4-mapped IPv6 forms (I-2 fix) normalize-then-classify.
func TestIsBlockedIP_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		ip      string
		blocked bool
	}{
		// Loopback
		{"loopback ipv4 127.0.0.1", "127.0.0.1", true},
		{"loopback ipv4 127.5.6.7", "127.5.6.7", true},
		{"loopback ipv6 ::1", "::1", true},
		// RFC 1918 (IPv4 private)
		{"rfc1918 10/8", "10.1.2.3", true},
		{"rfc1918 172.16", "172.16.0.1", true},
		{"rfc1918 172.31", "172.31.255.254", true},
		{"rfc1918 192.168", "192.168.1.1", true},
		// RFC 4193 IPv6 ULA
		{"rfc4193 fc00::", "fc00::1", true},
		{"rfc4193 fdff::", "fdff::1", true},
		// Link-local
		{"link-local ipv4 169.254", "169.254.169.254", true}, // AWS/GCP cloud metadata
		{"link-local ipv6 fe80::", "fe80::1", true},
		// Multicast
		{"multicast ipv4 224", "224.0.0.1", true},
		{"multicast ipv6 ff02::", "ff02::1", true},
		// CGNAT (RFC 6598)
		{"cgnat 100.64", "100.64.0.1", true},
		{"cgnat 100.127", "100.127.255.254", true},
		// Unspecified / "this network"
		{"unspecified ipv4", "0.0.0.0", true},
		{"unspecified ipv6", "::", true},
		{"this-network 0.1.2.3", "0.1.2.3", true},
		// Broadcast
		{"broadcast", "255.255.255.255", true},
		// I-2 fix: IPv4-mapped IPv6 (::ffff:*) normalized to v4 before classification.
		{"ipv4-mapped loopback", "::ffff:127.0.0.1", true},
		{"ipv4-mapped rfc1918", "::ffff:10.0.0.1", true},
		{"ipv4-mapped link-local", "::ffff:169.254.169.254", true},
		{"ipv4-mapped cgnat", "::ffff:100.64.0.1", true},
		{"ipv4-mapped public allowed", "::ffff:8.8.8.8", false},
		// PUBLIC — must NOT be blocked
		{"public ipv4 8.8.8.8", "8.8.8.8", false},
		{"public ipv4 1.1.1.1", "1.1.1.1", false},
		{"public ipv6 google", "2001:4860:4860::8888", false},
		// Edge of CGNAT (just outside)
		{"public ipv4 100.63.255.254", "100.63.255.254", false},
		{"public ipv4 100.128.0.1", "100.128.0.1", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) returned nil", tc.ip)
			}
			if got := isBlockedIP(ip); got != tc.blocked {
				t.Errorf("isBlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

// TestIsBlockedIP_NilIsBlocked — defensive nil-guard.
func TestIsBlockedIP_NilIsBlocked(t *testing.T) {
	if !isBlockedIP(nil) {
		t.Errorf("isBlockedIP(nil) returned false; want true (defensive)")
	}
}

// TestSSRFGuard_DialContext_RejectsLoopbackLiteral — IP-literal "127.0.0.1" is
// rejected without a successful TCP connect: LookupIPAddr accepts the literal,
// returns it unchanged, isBlockedIP catches it, error wraps ErrSSRFRejected.
func TestSSRFGuard_DialContext_RejectsLoopbackLiteral(t *testing.T) {
	g := newSSRFGuard()
	_, err := g.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil {
		t.Fatal("expected error dialing 127.0.0.1; got nil")
	}
	if !errors.Is(err, ErrSSRFRejected) {
		t.Errorf("error does not wrap ErrSSRFRejected: %v", err)
	}
}

// TestSSRFGuard_DialContext_RejectsRFC1918Literal — same path for private IPs.
func TestSSRFGuard_DialContext_RejectsRFC1918Literal(t *testing.T) {
	g := newSSRFGuard()
	_, err := g.DialContext(context.Background(), "tcp", "10.0.0.1:443")
	if err == nil || !errors.Is(err, ErrSSRFRejected) {
		t.Errorf("expected ErrSSRFRejected for 10.0.0.1; got %v", err)
	}
}

// TestSSRFGuard_DialContext_RejectsLinkLocalLiteral — cloud metadata IP
// (AWS/GCP IAM credential exfiltration target).
func TestSSRFGuard_DialContext_RejectsLinkLocalLiteral(t *testing.T) {
	g := newSSRFGuard()
	_, err := g.DialContext(context.Background(), "tcp", "169.254.169.254:80")
	if err == nil || !errors.Is(err, ErrSSRFRejected) {
		t.Errorf("expected ErrSSRFRejected for 169.254.169.254; got %v", err)
	}
}

// TestSSRFGuard_DialContext_RejectsCGNATLiteral — 100.64/10 not covered by stdlib.
func TestSSRFGuard_DialContext_RejectsCGNATLiteral(t *testing.T) {
	g := newSSRFGuard()
	_, err := g.DialContext(context.Background(), "tcp", "100.64.0.1:443")
	if err == nil || !errors.Is(err, ErrSSRFRejected) {
		t.Errorf("expected ErrSSRFRejected for 100.64.0.1 (CGNAT); got %v", err)
	}
}

// TestSSRFGuard_DialContext_RejectsIPv4MappedLoopback — I-2 fix: attacker uses
// the bracket-IPv6 mapped form to bypass v4-only classifiers. The address
// SplitHostPort emits is "::ffff:127.0.0.1" which net.ParseIP returns as a
// 16-byte slice; isBlockedIP normalizes via To4 before classification.
func TestSSRFGuard_DialContext_RejectsIPv4MappedLoopback(t *testing.T) {
	g := newSSRFGuard()
	_, err := g.DialContext(context.Background(), "tcp", "[::ffff:127.0.0.1]:80")
	if err == nil || !errors.Is(err, ErrSSRFRejected) {
		t.Errorf("expected ErrSSRFRejected for ::ffff:127.0.0.1; got %v", err)
	}
}

// TestValidateCallbackURL_Cases — pre-flight rejection. No DNS lookups happen
// in this path (stage 1 of two-stage rejection); a hostname like
// "customer.example" passes pre-flight and is later validated at dial time.
func TestValidateCallbackURL_Cases(t *testing.T) {
	cases := []struct {
		name  string
		url   string
		errIs error // expected sentinel via errors.Is; nil means "no error"
	}{
		{"empty", "", ErrEmptyURL},
		{"http scheme", "http://customer.example/cb", ErrNonHTTPS},
		{"https scheme caps", "HTTPS://customer.example/cb", nil}, // case-insensitive scheme
		{"literal localhost", "https://localhost/cb", ErrSSRFRejected},
		{"literal Localhost case-insensitive", "https://LOCALHOST/cb", ErrSSRFRejected},
		{"literal 127.0.0.1", "https://127.0.0.1/cb", ErrSSRFRejected},
		{"literal 10.0.0.1", "https://10.0.0.1/cb", ErrSSRFRejected},
		{"literal 169.254.169.254", "https://169.254.169.254/cb", ErrSSRFRejected},
		{"literal 100.64.0.1", "https://100.64.0.1/cb", ErrSSRFRejected},
		{"literal ::1", "https://[::1]/cb", ErrSSRFRejected},
		{"literal fe80::", "https://[fe80::1]/cb", ErrSSRFRejected},
		// I-2 fix: IPv4-mapped IPv6 literals must reject just like the v4 form.
		{"literal ::ffff:127.0.0.1", "https://[::ffff:127.0.0.1]/cb", ErrSSRFRejected},
		{"literal ::ffff:10.0.0.1", "https://[::ffff:10.0.0.1]/cb", ErrSSRFRejected},
		{"literal ::ffff:169.254.169.254", "https://[::ffff:169.254.169.254]/cb", ErrSSRFRejected},
		{"public hostname", "https://customer.example/cb", nil},
		{"public hostname with port", "https://customer.example:8443/cb", nil},
		{"public hostname with path+query", "https://customer.example/cb?x=1&y=2", nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCallbackURL(tc.url)
			switch {
			case tc.errIs == nil && err != nil:
				t.Errorf("ValidateCallbackURL(%q) returned %v; want nil", tc.url, err)
			case tc.errIs != nil && !errors.Is(err, tc.errIs):
				t.Errorf("ValidateCallbackURL(%q) = %v; want errors.Is(_, %v)", tc.url, err, tc.errIs)
			}
		})
	}
}

// TestSSRFGuard_DialContext_DNSPathRunsForHostnames proves Pitfall 5
// mitigation: dialing a hostname runs net.DefaultResolver.LookupIPAddr, which
// returns the host's resolved IPs (e.g. localhost → 127.0.0.1 / ::1 from
// /etc/hosts), and the validation step rejects.
func TestSSRFGuard_DialContext_DNSPathRunsForHostnames(t *testing.T) {
	g := newSSRFGuard()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := g.DialContext(ctx, "tcp", "localhost:80")
	if err == nil {
		t.Fatal("expected error dialing localhost; got nil")
	}
	if !errors.Is(err, ErrSSRFRejected) {
		t.Errorf("error does not wrap ErrSSRFRejected: %v", err)
	}
	// Loose substring assertion — the wrapped error mentions the resolved IP
	// (likely 127.0.0.1 or ::1 depending on /etc/hosts), proving DNS ran.
	msg := err.Error()
	if !strings.Contains(msg, "127.") && !strings.Contains(msg, "::1") {
		t.Logf("warning: error message %q does not mention loopback IP — review on flaky CI", msg)
	}
}

// TestSSRFGuard_DialContext_AllowsPublicIPLiteral — sanity check that the
// guard does not reject 8.8.8.8 (we don't actually complete the TCP connect
// here — short timeout is fine; the assertion is "no SSRF error", not
// "successful TCP connect").
func TestSSRFGuard_DialContext_AllowsPublicIPLiteral(t *testing.T) {
	g := newSSRFGuard()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := g.DialContext(ctx, "tcp", "8.8.8.8:443")
	if err != nil && errors.Is(err, ErrSSRFRejected) {
		t.Fatalf("8.8.8.8 wrongly rejected as SSRF: %v", err)
	}
	// err can be context.DeadlineExceeded or nil (CI may complete connect) —
	// both acceptable; we only fail if SSRF rejection fired.
}
