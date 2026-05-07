package identity

import "testing"

func TestNewCallSidFormat(t *testing.T) {
	sid := NewCallSid()
	if !CallSidRE.MatchString(sid) {
		t.Fatalf("CallSid %q does not match ^CA[0-9a-f]{32}$", sid)
	}
	if len(sid) != 34 {
		t.Fatalf("CallSid length = %d, want 34", len(sid))
	}
}

func TestNewCallSidUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		sid := NewCallSid()
		if _, dup := seen[sid]; dup {
			t.Fatalf("duplicate CallSid %q on iteration %d", sid, i)
		}
		seen[sid] = struct{}{}
	}
	if len(seen) != 1000 {
		t.Fatalf("expected 1000 unique CallSids, got %d", len(seen))
	}
}

func TestCallSidREMatches(t *testing.T) {
	valid := []string{
		"CA" + "0123456789abcdef0123456789abcdef",
		NewCallSid(),
	}
	invalid := []string{
		"CA0123456789ABCDEF0123456789ABCDEF",   // uppercase
		"ACdeadbeefdeadbeefdeadbeefdeadbeef",   // wrong prefix (AC instead of CA)
		"CA0123456789abcdef0123456789abcd",     // too short
		"CA0123456789abcdef0123456789abcdefgg", // non-hex
	}
	for _, s := range valid {
		if !CallSidRE.MatchString(s) {
			t.Fatalf("expected %q to match CallSidRE", s)
		}
	}
	for _, s := range invalid {
		if CallSidRE.MatchString(s) {
			t.Fatalf("expected %q NOT to match CallSidRE", s)
		}
	}
}
