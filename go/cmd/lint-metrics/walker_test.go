package main

import (
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// loadFixture loads one or more package patterns rooted at testdata/* via
// packages.Load with the same Mode the production main.go uses.
func loadFixture(t *testing.T, patterns ...string) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports | packages.NeedCompiledGoFiles,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("packages.Load: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatalf("packages.Load reported package errors")
	}
	if len(pkgs) == 0 {
		t.Fatalf("packages.Load returned no packages for patterns %v", patterns)
	}
	return pkgs
}

// runWalker walks every supplied package and returns the resulting walker.
func runWalker(pkgs []*packages.Package) *Walker {
	w := NewWalker()
	for _, p := range pkgs {
		w.WalkPackage(p)
	}
	return w
}

// diagnosticsForFile returns only those diagnostics whose Position.Filename
// has the supplied basename. Used to scope an assertion to a single fixture
// file inside a multi-file fixture package.
func diagnosticsForFile(diags []Diagnostic, basename string) []Diagnostic {
	var out []Diagnostic
	for _, d := range diags {
		if strings.HasSuffix(d.Position.Filename, "/"+basename) || strings.HasSuffix(d.Position.Filename, "\\"+basename) {
			out = append(out, d)
		}
	}
	return out
}

// ─── Test 1: clean fixture → 0 diagnostics ────────────────────────────────

func TestWalker_CleanFixture_NoViolations(t *testing.T) {
	pkgs := loadFixture(t, "./testdata/clean/...")
	w := runWalker(pkgs)
	if w.HasErrors() {
		t.Errorf("expected 0 diagnostics on clean fixture, got %d:", len(w.diagnostics))
		for _, d := range w.diagnostics {
			t.Errorf("  %s", d)
		}
	}
}

// ─── Tests 2-8 + 11 + 13: violation fixtures ──────────────────────────────

func TestWalker_Violations(t *testing.T) {
	pkgs := loadFixture(t, "./testdata/violations/...")
	w := runWalker(pkgs)
	if !w.HasErrors() {
		t.Fatalf("expected diagnostics on violations fixture, got 0")
	}

	cases := []struct {
		name      string
		basename  string
		wantSubstr string
	}{
		{"raw_status_code", "raw_status_code.go", "raw HTTP status code"},
		{"e164_phone", "e164_phone.go", "phone number pattern"},
		{"callsid_label", "callsid_label.go", "CallSid pattern"},
		{"url_label", "url_label.go", "URL pattern"},
		{"accountsid_label", "accountsid_label.go", "AccountSid pattern"},
		{"local_var_from_param", "local_var_from_param.go", "function-param source"},
		{"literal_outside_allowlist", "literal_outside_allowlist.go", "not in allowlist"},
		{"log_phone_url_at_info", "log_phone_url_at_info.go", `Info+ log emits Str("`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := diagnosticsForFile(w.diagnostics, tc.basename)
			if len(diags) == 0 {
				t.Fatalf("expected ≥1 diagnostic in %s, got 0", tc.basename)
			}
			matched := false
			for _, d := range diags {
				if strings.Contains(d.Reason, tc.wantSubstr) {
					matched = true
					break
				}
			}
			if !matched {
				t.Errorf("expected a diagnostic containing %q in %s; got:", tc.wantSubstr, tc.basename)
				for _, d := range diags {
					t.Errorf("  %s", d)
				}
			}
		})
	}
}

// ─── Test 9: bucketer call accepted ───────────────────────────────────────

func TestWalker_BucketerCallAccepted(t *testing.T) {
	pkgs := loadFixture(t, "./testdata/clean/...")
	w := runWalker(pkgs)
	diags := diagnosticsForFile(w.diagnostics, "bucketer_call_label.go")
	if len(diags) != 0 {
		t.Errorf("expected 0 diagnostics in bucketer_call_label.go, got %d:", len(diags))
		for _, d := range diags {
			t.Errorf("  %s", d)
		}
	}
}

// ─── Test 10: local var from bucketer accepted ────────────────────────────

func TestWalker_LocalVarFromBucketerAccepted(t *testing.T) {
	pkgs := loadFixture(t, "./testdata/clean/...")
	w := runWalker(pkgs)
	diags := diagnosticsForFile(w.diagnostics, "local_var_from_bucketer.go")
	if len(diags) != 0 {
		t.Errorf("expected 0 diagnostics in local_var_from_bucketer.go, got %d:", len(diags))
		for _, d := range diags {
			t.Errorf("  %s", d)
		}
	}
}

// ─── Test 12: enum-const ident accepted ───────────────────────────────────

func TestWalker_EnumConstAccepted(t *testing.T) {
	pkgs := loadFixture(t, "./testdata/clean/...")
	w := runWalker(pkgs)
	diags := diagnosticsForFile(w.diagnostics, "enum_const_label.go")
	if len(diags) != 0 {
		t.Errorf("expected 0 diagnostics in enum_const_label.go, got %d:", len(diags))
		for _, d := range diags {
			t.Errorf("  %s", d)
		}
	}
}

// ─── Test 14: dynamic-allowed + url-allowed opt-outs accepted ─────────────

func TestWalker_OptOutsAccepted(t *testing.T) {
	pkgs := loadFixture(t, "./testdata/clean/...")
	w := runWalker(pkgs)
	diags := diagnosticsForFile(w.diagnostics, "dynamic_allowed_optout.go")
	if len(diags) != 0 {
		t.Errorf("expected 0 diagnostics in dynamic_allowed_optout.go, got %d:", len(diags))
		for _, d := range diags {
			t.Errorf("  %s", d)
		}
	}
}

// ─── Test 15: real codebase clean ─────────────────────────────────────────
//
// Walks every production package and asserts 0 diagnostics. This is the
// load-bearing acceptance signal. Depends on the allowlist + bucketer
// comments AND on the few // metrics:dynamic-allowed annotations at the
// production non-literal call sites (api/server.go middleware,
// api/calls.go modify-outcome helper, webhook/status.go event-label,
// sip/forwarder.go recordFailure helper).
func TestWalker_RealCodebase_Clean(t *testing.T) {
	if testing.Short() {
		t.Skip("skip real-codebase walk in -short mode")
	}
	pkgs := loadFixture(t,
		"../../internal/observability/...",
		"../../internal/api/...",
		"../../internal/sip/...",
		"../../internal/bridge/...",
		"../../internal/webhook/...",
		"../../cmd/sipgate-sip-stream-bridge/...",
	)
	w := runWalker(pkgs)
	if w.HasErrors() {
		t.Errorf("walker found %d violations in real codebase:", len(w.diagnostics))
		for _, d := range w.diagnostics {
			t.Errorf("  %s", d)
		}
	}
}
