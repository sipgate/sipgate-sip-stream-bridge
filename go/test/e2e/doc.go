// Package e2e holds end-to-end integration tests covering cross-protocol
// invariants (WS + SIP + REST + log timestamps) that the sipp XML
// scenarios under tests/e2e/sipp/ cannot observe.
//
// All tests in this package are gated by `if testing.Short() { t.Skip(...) }`
// so `go test -short` (used during fast iteration) skips them. CI runs
// the full suite via the Makefile `e2e` target (see tests/e2e/sipp/README.md).
//
// Test files:
//   - e2e_shutdown_test.go       — SIGTERM-during-5-forwarded-calls
//   - e2e_privacy_gate_test.go   — 100-concurrent-<Dial> WS-stop-before-INVITE
//   - e2e_goroutine_leak_test.go — 100-call NumGoroutine baseline
//   - e2e_race_test.go           — CANCEL/BYE simultaneous race under -race
//
// Harness shape — single-file fallback:
//
//   - helpers.go is the single shared harness file. It exports the
//     mode-agnostic primitives (wsCaptureServer, sipDownstreamStub,
//     statusCallbackReceiver, runConcurrentCalls, goroutineBaselineDelta,
//     collectJSONLogLines, bridgeUnderTest interface) AND both
//     bring-up modes (in-process via newInProcessBridge for race /
//     privacy-gate / goroutine-leak; subprocess scaffolding via
//     newSubprocessBridge for the shutdown test — see helpers.go's
//     subprocess-mode docstring for why the subprocess is opt-in via
//     E2E_SUBPROCESS_BRIDGE env var, with the in-process simulated-SIGTERM
//     fallback documented inline).
//
// The two modes share the majority of state (the wsCaptureServer /
// sipDownstreamStub / statusCallbackReceiver setup is identical between
// modes; only the "drive a call lifecycle through real production code
// paths" segment differs, and that difference is small enough to keep
// within helpers.go behind the bridgeUnderTest interface).
package e2e
