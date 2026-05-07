package identity

import "testing"

func TestDeriveAccountSidFormat(t *testing.T) {
	sid := DeriveAccountSid("e12345p0")
	if !AccountSidRE.MatchString(sid) {
		t.Fatalf("AccountSid %q does not match ^AC[0-9a-f]{32}$", sid)
	}
	if len(sid) != 34 {
		t.Fatalf("AccountSid length = %d, want 34", len(sid))
	}
}

func TestDeriveAccountSidDeterministic(t *testing.T) {
	a := DeriveAccountSid("e12345p0")
	b := DeriveAccountSid("e12345p0")
	if a != b {
		t.Fatalf("DeriveAccountSid is non-deterministic: %q != %q", a, b)
	}
	c := DeriveAccountSid("different-user")
	if a == c {
		t.Fatalf("DeriveAccountSid produced same SID for distinct inputs: both = %q", a)
	}
}

func TestDeriveAccountSidEmptyInput(t *testing.T) {
	sid := DeriveAccountSid("")
	if !AccountSidRE.MatchString(sid) {
		t.Fatalf("AccountSid for empty SIP_USER %q does not match regex", sid)
	}
}

func TestAccountSidREMatches(t *testing.T) {
	valid := []string{
		"AC" + "0123456789abcdef0123456789abcdef",
		DeriveAccountSid("e12345p0"),
	}
	invalid := []string{
		"AC0123456789ABCDEF0123456789ABCDEF",   // uppercase rejected
		"CA0123456789abcdef0123456789abcdef",   // wrong prefix
		"AC0123456789abcdef0123456789abcd",     // 30 chars
		"AC0123456789abcdef0123456789abcdefgg", // non-hex
	}
	for _, s := range valid {
		if !AccountSidRE.MatchString(s) {
			t.Fatalf("expected %q to match AccountSidRE", s)
		}
	}
	for _, s := range invalid {
		if AccountSidRE.MatchString(s) {
			t.Fatalf("expected %q NOT to match AccountSidRE", s)
		}
	}
}
