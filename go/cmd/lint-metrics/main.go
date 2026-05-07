// Package main is the lint-metrics CI binary that enforces metric-label
// cardinality discipline. It walks every Go package in the supplied
// patterns (default ./...) and validates every prometheus *Vec WithLabelValues
// call site against the // metrics:allowlist comment annotations adjacent to
// the corresponding *Vec declaration in internal/observability/metrics.go.
//
// In addition to the metric-label visitor, a second AST visitor scans
// zerolog event-builder chains for Info+ level Str("from"|"to"|"url"|...)
// emits — enforcing the phone-number/URL debug-only convention with a
// CI-enforceable check.
//
// Exit codes:
//
//	0 — no violations
//	1 — at least one violation (diagnostics printed to stderr)
//	2 — packages.Load error (package patterns invalid, etc.)
package main

import (
	"fmt"
	"os"

	"golang.org/x/tools/go/packages"
)

func main() {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports | packages.NeedCompiledGoFiles,
		Tests: false,
	}
	patterns := os.Args[1:]
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "packages.Load: %v\n", err)
		os.Exit(2)
	}
	if packages.PrintErrors(pkgs) > 0 {
		os.Exit(2)
	}
	walker := NewWalker()
	for _, pkg := range pkgs {
		walker.WalkPackage(pkg)
	}
	if walker.HasErrors() {
		walker.PrintDiagnostics(os.Stderr)
		os.Exit(1)
	}
}
