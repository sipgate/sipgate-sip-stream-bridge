// walker.go — metric-label cardinality lint walker.
//
// Two AST visitors run over every supplied package:
//
//   1. Metric-label visitor (Pass 1+2): collects // metrics:allowlist
//      comments adjacent to prometheus.NewCounterVec / NewHistogramVec
//      declarations, then validates every *Vec.WithLabelValues(...) call
//      site against the parsed allowlist. Non-literal arguments are
//      classified by the multi-mode classifier in classifyLabelArg —
//      bucketer-call / local-var-from-bucketer / enum-const-ident / enum-
//      const-selector are all accepted; function-param sources, struct-
//      field loads, and unknown call targets are flagged unless a
//      // metrics:dynamic-allowed opt-out comment immediately precedes
//      the call site.
//
//   2. Log-field visitor (Pass 3): walks every zerolog event-builder
//      chain terminated by .Msg(...) or .Send(). At Info+ levels, an
//      Str("from"|"to"|"url"|"caller"|"callee"|"to_uri"|"from_uri"|
//      "target_url"|"callback_url", _) emit is a violation unless the
//      chain carries a // metrics:url-allowed opt-out (security-event
//      escape hatch).
//
// Both visitors share the same Diagnostic surface and emit through the
// walker's accumulating diagnostics slice.
//
// Cross-package bucketer/enum-const resolution is supported because pass 0
// runs against every package the caller queues (via WalkPackage) BEFORE
// pass 2/3 runs — the walker buffers packages and runs the full pipeline
// on the first HasErrors / PrintDiagnostics call.

package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"io"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

var (
	// Leak-pattern regexes — used to flag literal label values that look
	// like phone numbers / URLs / Twilio identifiers / raw status codes.
	e164Re       = regexp.MustCompile(`^\+?[0-9]{8,}$`)
	urlRe        = regexp.MustCompile(`^https?://`)
	accountSidRe = regexp.MustCompile(`^AC[0-9a-f]{32}$`)
	callSidRe    = regexp.MustCompile(`^CA[0-9a-f]{32}$`)
	rawStatusRe  = regexp.MustCompile(`^[1-5][0-9]{2}$`)

	// Comment annotation parsers. Each matches the comment text *with* the
	// `//` prefix preserved (we hand-strip whitespace before the prefix at
	// parse time so the regex byte-match is stable across formatters).
	allowlistRe  = regexp.MustCompile(`^//\s*metrics:allowlist\s+(.+)$`)
	bucketerRe   = regexp.MustCompile(`^//\s*metrics:bucketer\b`)
	dynAllowedRe = regexp.MustCompile(`^//\s*metrics:dynamic-allowed\b`)
	urlAllowedRe = regexp.MustCompile(`^//\s*metrics:url-allowed\b`)

	// Log-field visitor data (Pass 3).
	logLeakFieldNames = map[string]bool{
		"from":         true,
		"to":           true,
		"url":          true,
		"caller":       true,
		"callee":       true,
		"to_uri":       true,
		"from_uri":     true,
		"target_url":   true,
		"callback_url": true,
	}
	zerologInfoPlusLevels = map[string]bool{
		"Info":  true,
		"Warn":  true,
		"Error": true,
		"Fatal": true,
		"Panic": true,
	}
	zerologAllLevels = map[string]bool{
		"Trace": true, "Debug": true,
		"Info": true, "Warn": true, "Error": true, "Fatal": true, "Panic": true,
	}
)

// Diagnostic describes a single violation found by the walker.
type Diagnostic struct {
	Position token.Position // file:line:col
	Vector   string         // *Vec receiver name (or "" for log-field diagnostics)
	Label    string         // metric label name OR log Str() field name
	Value    string         // static-resolved value (or "" if dynamic)
	Reason   string         // human-readable explanation
}

func (d Diagnostic) String() string {
	return fmt.Sprintf("%s: vector=%s label=%q value=%q: %s",
		d.Position, d.Vector, d.Label, d.Value, d.Reason)
}

// Walker accumulates diagnostics across one or more packages.
//
// Usage: caller invokes WalkPackage(p) once per package; the first call
// to HasErrors / PrintDiagnostics triggers the full pipeline (passes 0
// through 3) across all queued packages. This buffering is what lets
// pass-2 cross-package bucketer / enum-const lookup work without a
// separate Finalize() call.
type Walker struct {
	pkgs []*packages.Package

	// Populated lazily on first finalize() call.
	finalized   bool
	diagnostics []Diagnostic

	// Pass 0 outputs.
	bucketers  map[string]bool   // qualified function name "<pkgPath>.<Name>" → true
	enumConsts map[string]string // qualified const name "<pkgPath>.<Name>" → string value

	// Pass 1 outputs — one entry per *Vec declaration in any walked pkg.
	allowlists map[*types.Var]map[string][]string // vector var → label → allowed values
	labelOrder map[*types.Var][]string            // vector var → ordered label names
	vectorName map[*types.Var]string              // vector var → identifier (for diagnostics)
}

// NewWalker constructs an empty Walker.
func NewWalker() *Walker {
	return &Walker{
		bucketers:  make(map[string]bool),
		enumConsts: make(map[string]string),
		allowlists: make(map[*types.Var]map[string][]string),
		labelOrder: make(map[*types.Var][]string),
		vectorName: make(map[*types.Var]string),
	}
}

// WalkPackage queues a package for the walker. Actual analysis happens
// lazily on the first HasErrors / PrintDiagnostics call so cross-package
// bucketer / enum-const lookup works regardless of the order the caller
// queues packages in.
func (w *Walker) WalkPackage(pkg *packages.Package) {
	if pkg == nil || pkg.Syntax == nil {
		return
	}
	w.pkgs = append(w.pkgs, pkg)
}

// HasErrors finalises the walker (passes 0-3) on first call and reports
// whether any diagnostics were collected.
func (w *Walker) HasErrors() bool {
	w.finalize()
	return len(w.diagnostics) > 0
}

// PrintDiagnostics writes the (sorted-by-position) accumulated diagnostics
// to out. Finalises the walker on first call.
func (w *Walker) PrintDiagnostics(out io.Writer) {
	w.finalize()
	sort.SliceStable(w.diagnostics, func(i, j int) bool {
		a, b := w.diagnostics[i].Position, w.diagnostics[j].Position
		if a.Filename != b.Filename {
			return a.Filename < b.Filename
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	for _, d := range w.diagnostics {
		fmt.Fprintln(out, d)
	}
}

// finalize runs the full 4-pass pipeline once.
func (w *Walker) finalize() {
	if w.finalized {
		return
	}
	w.finalized = true
	// Pass 0: bucketer + enum-const collection (every queued pkg).
	for _, p := range w.pkgs {
		w.collectBucketers(p)
		w.collectEnumConsts(p)
	}
	// Pass 1: vector declarations + allowlist parsing.
	for _, p := range w.pkgs {
		w.collectVectorDeclarations(p)
	}
	// Pass 2: WithLabelValues call-site classification.
	for _, p := range w.pkgs {
		w.checkMetricCallSites(p)
	}
	// Pass 3: log-field visitor.
	for _, p := range w.pkgs {
		w.checkLogChains(p)
	}
}

// ─── Pass 0 ───────────────────────────────────────────────────────────────

// collectBucketers walks every top-level FuncDecl in pkg and records those
// preceded by a // metrics:bucketer comment. Verifies the function returns
// exactly one string — emits a Diagnostic otherwise.
func (w *Walker) collectBucketers(pkg *packages.Package) {
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if !w.docHasComment(fd.Doc, bucketerRe) {
				continue
			}
			obj := pkg.TypesInfo.Defs[fd.Name]
			fn, _ := obj.(*types.Func)
			if fn == nil {
				continue
			}
			sig, _ := fn.Type().(*types.Signature)
			if sig == nil || sig.Results().Len() != 1 {
				w.diagnostics = append(w.diagnostics, Diagnostic{
					Position: pkg.Fset.Position(fd.Pos()),
					Vector:   fn.Name(),
					Reason:   "// metrics:bucketer annotation requires exactly one string return",
				})
				continue
			}
			ret := sig.Results().At(0).Type()
			basic, _ := ret.Underlying().(*types.Basic)
			if basic == nil || basic.Kind() != types.String {
				w.diagnostics = append(w.diagnostics, Diagnostic{
					Position: pkg.Fset.Position(fd.Pos()),
					Vector:   fn.Name(),
					Reason:   "// metrics:bucketer annotation requires exactly one string return",
				})
				continue
			}
			qname := pkg.PkgPath + "." + fn.Name()
			w.bucketers[qname] = true
		}
	}
}

// collectEnumConsts walks every top-level GenDecl with Tok==CONST and
// records string-typed const values keyed by qualified name.
func (w *Walker) collectEnumConsts(pkg *packages.Package) {
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, _ := spec.(*ast.ValueSpec)
				if vs == nil {
					continue
				}
				for _, name := range vs.Names {
					obj := pkg.TypesInfo.Defs[name]
					c, _ := obj.(*types.Const)
					if c == nil {
						continue
					}
					if v, ok := enumConstString(c); ok {
						w.enumConsts[pkg.PkgPath+"."+c.Name()] = v
					}
				}
			}
		}
	}
}

// docHasComment reports whether any line in the comment group matches re.
func (w *Walker) docHasComment(cg *ast.CommentGroup, re *regexp.Regexp) bool {
	if cg == nil {
		return false
	}
	for _, c := range cg.List {
		if re.MatchString(strings.TrimSpace(c.Text)) {
			return true
		}
	}
	return false
}

// ─── Pass 1 ───────────────────────────────────────────────────────────────

// collectVectorDeclarations walks every package looking for *Vec
// declarations. Two declaration shapes are supported:
//
//  1. Top-level `var name = prometheus.NewCounterVec(...)` (with a
//     // metrics:allowlist comment on either GenDecl.Doc or ValueSpec.Doc).
//
//  2. Inside-function `name := prometheus.NewCounterVec(...)` (with a
//     // metrics:allowlist line-comment immediately above the AssignStmt).
//     This is the production shape — internal/observability/metrics.go's
//     NewMetrics() defines all *Vec collectors as local variables before
//     returning them via struct-field assignment.
//
// In both cases the local *types.Var is recorded as the canonical handle
// and the struct-field-name → vector-name fallback in resolveReceiverVar
// matches caller-site selectors like `m.APIRequestsTotal.WithLabelValues(...)`.
func (w *Walker) collectVectorDeclarations(pkg *packages.Package) {
	for _, file := range pkg.Syntax {
		// Top-level var declarations.
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, _ := spec.(*ast.ValueSpec)
				if vs == nil {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					call, ok := vs.Values[i].(*ast.CallExpr)
					if !ok {
						continue
					}
					labels, kind := w.matchPromVecConstructor(call, pkg.TypesInfo)
					if kind == "" {
						continue
					}
					vobj, _ := pkg.TypesInfo.Defs[name].(*types.Var)
					if vobj == nil {
						continue
					}
					w.vectorName[vobj] = name.Name
					w.labelOrder[vobj] = labels
					var alDoc *ast.CommentGroup
					if vs.Doc != nil {
						alDoc = vs.Doc
					} else if gd.Doc != nil {
						alDoc = gd.Doc
					}
					w.allowlists[vobj] = w.parseAllowlistDoc(alDoc, labels)
				}
			}
		}
		// Inside-function `:=` *Vec assignments.
		w.collectVectorAssignsInFuncs(file, pkg)
	}
}

// collectVectorAssignsInFuncs scans every FuncDecl body for `name :=
// prometheus.NewCounterVec(...)` / `NewHistogramVec(...)` AssignStmts and
// records the local *types.Var. The // metrics:allowlist comment
// adjacency is detected via fset.Position(line) lookup on file.Comments
// — line N or N-1 relative to the AssignStmt's position.
func (w *Walker) collectVectorAssignsInFuncs(file *ast.File, pkg *packages.Package) {
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if assign.Tok != token.DEFINE && assign.Tok != token.ASSIGN {
			return true
		}
		for i, lhs := range assign.Lhs {
			if i >= len(assign.Rhs) {
				continue
			}
			call, ok := assign.Rhs[i].(*ast.CallExpr)
			if !ok {
				continue
			}
			labels, kind := w.matchPromVecConstructor(call, pkg.TypesInfo)
			if kind == "" {
				continue
			}
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			var vobj *types.Var
			if assign.Tok == token.DEFINE {
				vobj, _ = pkg.TypesInfo.Defs[id].(*types.Var)
			} else {
				vobj, _ = pkg.TypesInfo.ObjectOf(id).(*types.Var)
			}
			if vobj == nil {
				continue
			}
			w.vectorName[vobj] = id.Name
			w.labelOrder[vobj] = labels
			// Find the // metrics:allowlist line-comment immediately
			// above this AssignStmt (line N-1 or earlier in the same
			// continuous comment group).
			alDoc := w.findAdjacentDocAbove(file, pkg.Fset, assign.Pos())
			w.allowlists[vobj] = w.parseAllowlistDoc(alDoc, labels)
		}
		return true
	})
}

// findAdjacentDocAbove returns the comment group whose last line is on
// the line immediately above pos. Used for inside-function declarations
// that don't have an attached *ast.CommentGroup via the GenDecl/ValueSpec
// Doc fields.
func (w *Walker) findAdjacentDocAbove(file *ast.File, fset *token.FileSet, pos token.Pos) *ast.CommentGroup {
	target := fset.Position(pos).Line
	for _, cg := range file.Comments {
		// Comment group whose LAST line is target-1.
		lastLine := fset.Position(cg.End()).Line
		if lastLine == target-1 {
			return cg
		}
	}
	return nil
}

// matchPromVecConstructor checks whether call matches
// prometheus.NewCounterVec(opts, []string{labels...}) or NewHistogramVec.
// Returns the parsed label names + the constructor kind ("CounterVec" /
// "HistogramVec") or "" if not a match.
func (w *Walker) matchPromVecConstructor(call *ast.CallExpr, info *types.Info) ([]string, string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil, ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return nil, ""
	}
	// Validate the package is prometheus (ignore other "prometheus"-named
	// idents by checking the resolved object's import path).
	pn, _ := info.ObjectOf(x).(*types.PkgName)
	if pn == nil || pn.Imported().Path() != "github.com/prometheus/client_golang/prometheus" {
		return nil, ""
	}
	switch sel.Sel.Name {
	case "NewCounterVec":
	case "NewHistogramVec":
	default:
		return nil, ""
	}
	if len(call.Args) < 2 {
		return nil, sel.Sel.Name
	}
	// Second arg is `[]string{"a","b"}` literal.
	cl, ok := call.Args[1].(*ast.CompositeLit)
	if !ok {
		return nil, sel.Sel.Name
	}
	var labels []string
	for _, e := range cl.Elts {
		bl, ok := e.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			continue
		}
		labels = append(labels, strings.Trim(bl.Value, `"`))
	}
	return labels, sel.Sel.Name
}

// parseAllowlistDoc reads a comment group adjacent to a *Vec declaration
// and returns label→[]values. Each `// metrics:allowlist` line carries
// space-separated label=value-list tokens; multiple comment lines are
// merged.
func (w *Walker) parseAllowlistDoc(cg *ast.CommentGroup, labels []string) map[string][]string {
	out := make(map[string][]string)
	if cg == nil {
		return out
	}
	for _, c := range cg.List {
		text := strings.TrimSpace(c.Text)
		m := allowlistRe.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		// Split body into tokens; each token is `label=v1|v2|...`.
		for _, tok := range strings.Fields(m[1]) {
			eq := strings.IndexByte(tok, '=')
			if eq <= 0 {
				continue
			}
			label := tok[:eq]
			values := strings.Split(tok[eq+1:], "|")
			out[label] = append(out[label], values...)
		}
	}
	_ = labels // labels validated implicitly by lookup at call sites
	return out
}

// ─── Pass 2: WithLabelValues classifier ───────────────────────────────────

// checkMetricCallSites walks every CallExpr in pkg and dispatches every
// `<vec>.WithLabelValues(...)` to the multi-mode classifier.
//
// Tracks the enclosing FuncDecl with a single mutable `currentFn`
// overwritten on each *ast.FuncDecl. The nil post-order signal is not
// needed for stack-pop semantics; a future refactor that adds nested
// FuncDecl walking would want a real stack, but the current
// single-FuncDecl-per-decl invariant is correctly captured here.
func (w *Walker) checkMetricCallSites(pkg *packages.Package) {
	for _, file := range pkg.Syntax {
		// currentFn tracks the enclosing FuncDecl for local-var provenance.
		// Updated on every *ast.FuncDecl visit; reset to nil at file scope
		// implicitly by the per-file fresh declaration.
		var currentFn *ast.FuncDecl
		ast.Inspect(file, func(n ast.Node) bool {
			if fd, ok := n.(*ast.FuncDecl); ok {
				currentFn = fd
				return true
			}
			if call, ok := n.(*ast.CallExpr); ok {
				w.checkWithLabelValuesCall(call, pkg, file, currentFn)
			}
			return true
		})
	}
}

// checkWithLabelValuesCall classifies every arg of a *Vec.WithLabelValues
// call and emits diagnostics for non-accepted classifications. A no-op for
// non-WithLabelValues calls.
func (w *Walker) checkWithLabelValuesCall(call *ast.CallExpr, pkg *packages.Package, file *ast.File, fn *ast.FuncDecl) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithLabelValues" {
		return
	}
	// Resolve the receiver to a *types.Var (the vector declaration).
	vobj := w.resolveReceiverVar(sel.X, pkg.TypesInfo)
	if vobj == nil || w.allowlists[vobj] == nil {
		return // not a tracked *Vec receiver
	}
	labels := w.labelOrder[vobj]
	allow := w.allowlists[vobj]
	vname := w.vectorName[vobj]

	dynamicAllowed := w.lineHasOptOut(file, pkg.Fset, call.Lparen, dynAllowedRe)

	for i, arg := range call.Args {
		labelName := ""
		if i < len(labels) {
			labelName = labels[i]
		}
		c := w.classifyLabelArg(arg, fn, pkg, file)

		// Literal classification — if the value is in the allowlist, it
		// is authoritative: even if the byte-shape happens to match a
		// leak-pattern regex (e.g. "401" matches rawStatusRe but is the
		// declared kind enum for auth_challenge_kind_total) the literal
		// MUST be accepted. The allowlist is the contract.
		if c.static != "" {
			inAllowlist := allowlistContains(allow[labelName], c.static)
			if !inAllowlist {
				if reason, hit := w.literalLeakReason(c.static); hit {
					w.diagnostics = append(w.diagnostics, Diagnostic{
						Position: pkg.Fset.Position(arg.Pos()),
						Vector:   vname,
						Label:    labelName,
						Value:    c.static,
						Reason:   reason,
					})
					continue
				}
				w.diagnostics = append(w.diagnostics, Diagnostic{
					Position: pkg.Fset.Position(arg.Pos()),
					Vector:   vname,
					Label:    labelName,
					Value:    c.static,
					Reason:   fmt.Sprintf("value %q not in allowlist for label %q of vector %s", c.static, labelName, vname),
				})
				continue
			}
		}

		if !c.accepted {
			if dynamicAllowed {
				continue
			}
			w.diagnostics = append(w.diagnostics, Diagnostic{
				Position: pkg.Fset.Position(arg.Pos()),
				Vector:   vname,
				Label:    labelName,
				Reason:   c.reason,
			})
		}
	}
}

// allowlistContains is a simple linear membership check.
func allowlistContains(values []string, s string) bool {
	for _, v := range values {
		if v == s {
			return true
		}
	}
	return false
}

// resolveReceiverVar walks a SelectorExpr like `m.APIRequestsTotal` or
// `c.metrics.StatusCallbackAttemptsTotal` and returns the *types.Var
// corresponding to the *Vec declaration, or nil if the chain doesn't
// resolve to a known *Vec.
func (w *Walker) resolveReceiverVar(x ast.Expr, info *types.Info) *types.Var {
	switch e := x.(type) {
	case *ast.Ident:
		obj := info.ObjectOf(e)
		v, _ := obj.(*types.Var)
		if v != nil && w.allowlists[v] != nil {
			return v
		}
		return nil
	case *ast.SelectorExpr:
		// For `m.APIRequestsTotal` the SelectorExpr.Sel resolves to a
		// *types.Var on the Metrics struct field. We also try matching the
		// field's underlying var by looking up the field's type — but
		// that's not how *Vec references work. Instead, match by struct-
		// field name against any *Vec we recorded.
		if obj := info.ObjectOf(e.Sel); obj != nil {
			v, _ := obj.(*types.Var)
			if v != nil && w.allowlists[v] != nil {
				return v
			}
		}
		// Fall back: match by field-name → vector-name.
		fieldName := e.Sel.Name
		for vobj, name := range w.vectorName {
			// The Metrics struct field name typically differs in case
			// from the package-level var (APIRequestsTotal vs
			// apiRequestsTotal). Match either case-insensitively.
			if strings.EqualFold(name, fieldName) {
				return vobj
			}
		}
		return nil
	}
	return nil
}

// lineHasOptOut reports whether a comment matching re sits on a line
// adjacent to pos. "Adjacent" means: the comment's containing comment
// group ends on the line immediately preceding pos (group-doc form), OR
// the comment is on the same line as pos (trailing form). This handles
// multi-line `// metrics:dynamic-allowed ...` blocks where the marker
// line may be several lines above the call site.
func (w *Walker) lineHasOptOut(file *ast.File, fset *token.FileSet, pos token.Pos, re *regexp.Regexp) bool {
	target := fset.Position(pos).Line
	for _, cg := range file.Comments {
		groupEnd := fset.Position(cg.End()).Line
		groupContiguousAdjacent := groupEnd == target-1
		for _, c := range cg.List {
			cl := fset.Position(c.Pos()).Line
			sameLine := cl == target
			if !sameLine && !groupContiguousAdjacent {
				continue
			}
			if re.MatchString(strings.TrimSpace(c.Text)) {
				return true
			}
		}
	}
	return false
}

// literalLeakReason matches a literal label value against the leak-pattern
// regexes. Returns (reason, true) on a hit.
func (w *Walker) literalLeakReason(s string) (string, bool) {
	switch {
	case rawStatusRe.MatchString(s):
		return "raw HTTP status code detected (bucket via observability.BucketStatus)", true
	case accountSidRe.MatchString(s):
		return "AccountSid pattern detected (high-cardinality leak — never label by AC*)", true
	case callSidRe.MatchString(s):
		return "CallSid pattern detected (high-cardinality leak — never label by CA*)", true
	case urlRe.MatchString(s):
		return "URL pattern detected (high-cardinality leak — never label by URL)", true
	case e164Re.MatchString(s):
		return "phone number pattern detected (E.164 — high-cardinality leak)", true
	}
	return "", false
}

// classification is the output of classifyLabelArg.
type classification struct {
	accepted bool
	reason   string // populated when !accepted
	static   string // populated when arg resolves to a known string literal
}

// classifyLabelArg implements Detection Rule #3a.
func (w *Walker) classifyLabelArg(arg ast.Expr, fn *ast.FuncDecl, pkg *packages.Package, file *ast.File) classification {
	switch a := arg.(type) {
	case *ast.BasicLit:
		if a.Kind != token.STRING {
			return classification{accepted: false, reason: "non-string literal label value"}
		}
		return classification{accepted: true, static: strings.Trim(a.Value, `"`)}
	case *ast.CallExpr:
		obj := w.resolveCallTarget(a, pkg.TypesInfo)
		if obj == nil {
			return classification{accepted: false, reason: "non-literal label value (unresolvable function call)"}
		}
		qname := obj.Pkg().Path() + "." + obj.Name()
		if w.bucketers[qname] {
			return classification{accepted: true}
		}
		return classification{accepted: false, reason: fmt.Sprintf(
			"non-literal label value (function %s not annotated // metrics:bucketer; add the annotation if return is bounded, or use // metrics:dynamic-allowed)", qname)}
	case *ast.Ident:
		// Package-level enum const?
		if val, ok := w.lookupEnumConstIdent(a, pkg.TypesInfo); ok {
			return classification{accepted: true, static: val}
		}
		// Function param?
		if w.isFuncParam(a, fn, pkg.TypesInfo) {
			return classification{accepted: false, reason:
				"non-literal label value (function-param source; add // metrics:dynamic-allowed if intentional)"}
		}
		// Local variable — trace its assignment(s) within fn.
		sources := w.traceLocalVarSources(a, fn, pkg.TypesInfo)
		if len(sources) == 0 {
			return classification{accepted: false, reason:
				"non-literal label value (cannot trace local variable provenance; add // metrics:dynamic-allowed if intentional)"}
		}
		// All branches must classify as accepted.
		for _, src := range sources {
			sub := w.classifyLabelArg(src, fn, pkg, file)
			if !sub.accepted {
				return classification{accepted: false, reason:
					"non-literal label value (local variable assigned from " + sub.reason + ")"}
			}
		}
		return classification{accepted: true}
	case *ast.SelectorExpr:
		if val, ok := w.lookupEnumConstSelector(a, pkg.TypesInfo); ok {
			return classification{accepted: true, static: val}
		}
		return classification{accepted: false, reason:
			"non-literal label value (selector to non-const; add // metrics:dynamic-allowed if intentional)"}
	default:
		return classification{accepted: false, reason: "non-literal label value (add // metrics:dynamic-allowed if intentional)"}
	}
}

// resolveCallTarget returns the *types.Func behind a CallExpr, or nil.
func (w *Walker) resolveCallTarget(call *ast.CallExpr, info *types.Info) *types.Func {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		obj, _ := info.ObjectOf(fn).(*types.Func)
		return obj
	case *ast.SelectorExpr:
		obj, _ := info.ObjectOf(fn.Sel).(*types.Func)
		return obj
	}
	return nil
}

// lookupEnumConstIdent resolves an *ast.Ident to an enum-const value.
func (w *Walker) lookupEnumConstIdent(id *ast.Ident, info *types.Info) (string, bool) {
	c, _ := info.ObjectOf(id).(*types.Const)
	if c == nil {
		return "", false
	}
	return enumConstString(c)
}

// lookupEnumConstSelector resolves a `pkg.Const` selector to an enum-const
// value.
func (w *Walker) lookupEnumConstSelector(s *ast.SelectorExpr, info *types.Info) (string, bool) {
	c, _ := info.ObjectOf(s.Sel).(*types.Const)
	if c == nil {
		return "", false
	}
	return enumConstString(c)
}

// enumConstString returns the string value of a string-typed const, or
// ("", false) if the const is not string-typed.
//
// Both `string` and `untyped string` constants are accepted — Go untyped
// const declarations like `const EventInitiated = "initiated"` carry
// types.UntypedString rather than types.String. Both are bounded at the
// declaration site so both are equally trustworthy as label values.
func enumConstString(c *types.Const) (string, bool) {
	basic, _ := c.Type().Underlying().(*types.Basic)
	if basic == nil {
		return "", false
	}
	if basic.Kind() != types.String && basic.Kind() != types.UntypedString {
		return "", false
	}
	if c.Val().Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(c.Val()), true
}

// isFuncParam reports whether ident resolves to a parameter of fn.
func (w *Walker) isFuncParam(id *ast.Ident, fn *ast.FuncDecl, info *types.Info) bool {
	if fn == nil || fn.Type == nil || fn.Type.Params == nil {
		return false
	}
	obj := info.ObjectOf(id)
	v, _ := obj.(*types.Var)
	if v == nil {
		return false
	}
	for _, field := range fn.Type.Params.List {
		for _, n := range field.Names {
			pobj := info.Defs[n]
			if pobj == v {
				return true
			}
		}
	}
	return false
}

// traceLocalVarSources walks fn.Body and returns every RHS expression
// assigned to ident's underlying *types.Var (within fn). This handles
// both `:=` (AssignStmt with token.DEFINE) and reassignment via `=`
// (AssignStmt with token.ASSIGN). Range-statement bindings yield the
// range source's value-element if applicable.
func (w *Walker) traceLocalVarSources(id *ast.Ident, fn *ast.FuncDecl, info *types.Info) []ast.Expr {
	if fn == nil || fn.Body == nil {
		return nil
	}
	target := info.ObjectOf(id)
	v, _ := target.(*types.Var)
	if v == nil {
		return nil
	}
	var sources []ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			// `lhs := rhs` or `lhs = rhs`. Only single-LHS / single-RHS
			// patterns are traced; multi-return assignments are treated
			// as untraceable (returning empty sources here would falsely
			// reject them; instead we return them as the rhs, letting
			// classifyLabelArg report "unresolvable function call" for
			// the call-form rhs).
			for i, lhs := range s.Lhs {
				lid, ok := lhs.(*ast.Ident)
				if !ok {
					continue
				}
				lobj := info.ObjectOf(lid)
				if lobj == nil {
					// On `:=` the new identifier's def lives in info.Defs.
					lobj = info.Defs[lid]
				}
				if lobj == nil || lobj != v {
					continue
				}
				if i < len(s.Rhs) {
					sources = append(sources, s.Rhs[i])
				} else if len(s.Rhs) == 1 {
					sources = append(sources, s.Rhs[0])
				}
			}
		case *ast.RangeStmt:
			// `for k, v := range src` — if our ident is the value, the
			// source is the range expression. We treat range-over-bounded
			// (an enum slice) as untraceable here; conservative fallback.
			if s.Value != nil {
				if vid, ok := s.Value.(*ast.Ident); ok {
					if info.Defs[vid] == v {
						sources = append(sources, s.X)
					}
				}
			}
			if s.Key != nil {
				if kid, ok := s.Key.(*ast.Ident); ok {
					if info.Defs[kid] == v {
						sources = append(sources, s.X)
					}
				}
			}
		}
		return true
	})
	return sources
}

// ─── Pass 3: zerolog log-field visitor ────────────────────────────────────

// checkLogChains walks every CallExpr ending in .Msg(...) or .Send() and
// flags Info+ Str("from"|"to"|"url"|...) emits without // metrics:url-allowed.
func (w *Walker) checkLogChains(pkg *packages.Package) {
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Msg" && sel.Sel.Name != "Msgf" && sel.Sel.Name != "Send" {
				return true
			}

			// Walk back through the chain to collect Str() invocations
			// AND identify the chain's level.
			level, strCalls := walkZerologChain(call)
			if level == "" {
				return true // not a zerolog chain
			}
			if !zerologInfoPlusLevels[level] {
				return true // Trace/Debug — allowed to leak phone/URL
			}
			if w.lineHasOptOut(file, pkg.Fset, call.Lparen, urlAllowedRe) {
				return true
			}
			for _, sc := range strCalls {
				name := w.zerologFieldName(sc, pkg.TypesInfo)
				if name == "" {
					continue
				}
				if !logLeakFieldNames[name] {
					continue
				}
				w.diagnostics = append(w.diagnostics, Diagnostic{
					Position: pkg.Fset.Position(sc.Pos()),
					Label:    name,
					Reason: fmt.Sprintf(
						`Info+ log emits Str(%q, _) which is a phone-number/URL field; demote to Debug or add // metrics:url-allowed (security-event opt-out)`, name),
				})
			}
			return true
		})
	}
}

// walkZerologChain walks back from a `.Msg(...)`-terminated CallExpr,
// returning the level (Trace/Debug/Info/Warn/Error/Fatal/Panic) and the
// list of Str() CallExprs in the chain. Returns ("", nil) when the chain
// is not a recognisable zerolog event-builder pattern.
func walkZerologChain(msgCall *ast.CallExpr) (string, []*ast.CallExpr) {
	var strCalls []*ast.CallExpr
	cur := msgCall.Fun.(*ast.SelectorExpr).X
	for {
		switch e := cur.(type) {
		case *ast.CallExpr:
			selSel, ok := e.Fun.(*ast.SelectorExpr)
			if !ok {
				return "", nil
			}
			name := selSel.Sel.Name
			if name == "Str" || name == "Stringer" {
				if name == "Str" {
					strCalls = append(strCalls, e)
				}
				cur = selSel.X
				continue
			}
			// Could be a level method (.Info() / .Warn() / ...). The level
			// method takes no args; verify.
			if zerologAllLevels[name] && len(e.Args) == 0 {
				return name, strCalls
			}
			// Or a chained builder method that we treat as transparent
			// (Err, Int, Dur, Bool, Bytes, Hex, Caller, Time, etc.).
			cur = selSel.X
			continue
		case *ast.SelectorExpr:
			// Reached the root receiver — the chain isn't terminated by a
			// recognisable level call. Common shape:
			//   logger := log.With()...Logger()  // sublogger
			//   logger.Info().Str(...).Msg(...)  // <- Info() seen above
			//
			// If we get here we've walked off the chain without finding a
			// level call — try one more step in case the receiver is
			// something like `log.Info()`.
			return "", nil
		case *ast.Ident:
			return "", nil
		default:
			return "", nil
		}
	}
}

// zerologFieldName extracts the literal first-argument string of a
// Str(name, _) CallExpr. Returns "" when name is not a literal or known
// enum-const.
func (w *Walker) zerologFieldName(call *ast.CallExpr, info *types.Info) string {
	if len(call.Args) == 0 {
		return ""
	}
	switch a := call.Args[0].(type) {
	case *ast.BasicLit:
		if a.Kind != token.STRING {
			return ""
		}
		return strings.Trim(a.Value, `"`)
	case *ast.Ident:
		if v, ok := w.lookupEnumConstIdent(a, info); ok {
			return v
		}
	case *ast.SelectorExpr:
		if v, ok := w.lookupEnumConstSelector(a, info); ok {
			return v
		}
	}
	return ""
}
