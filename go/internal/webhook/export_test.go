package webhook

// PerCallCountForTest reports the number of active per-CallSid worker
// states. Lives in a *_test.go file (excluded from production binaries
// by the Go build system) so it can be called from sibling webhook-
// package _test.go files (status_leak_test.go) without exposing the
// accessor on the production API surface (test-only access points
// must not appear in the production binary).
//
// Used by TestStatusClient_HighChurn_PerCallStateReturnsToZero in
// status_leak_test.go.
func (c *StatusClient) PerCallCountForTest() int {
	n := 0
	c.perCall.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
