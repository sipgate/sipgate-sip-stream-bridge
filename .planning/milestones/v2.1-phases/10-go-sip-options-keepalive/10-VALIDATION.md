---
phase: 10
slug: go-sip-options-keepalive
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-05
---

# Phase 10 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | go test |
| **Config file** | none — standard Go test toolchain |
| **Quick run command** | `go test ./go/internal/sip/... ./go/internal/config/...` |
| **Full suite command** | `go test -race ./go/internal/sip/... ./go/internal/config/... ./go/internal/observability/...` |
| **Estimated runtime** | ~5 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./go/internal/sip/...`
- **After every plan wave:** Run `go test -race ./go/internal/sip/... ./go/internal/config/... ./go/internal/observability/...`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** ~5 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 10-01-01 | 01 | 1 | OPTS-05 | unit | `go test ./go/internal/config/...` | ✅ | ⬜ pending |
| 10-01-02 | 01 | 1 | OPTS-01–05 | unit | `go test ./go/internal/observability/...` | ✅ | ⬜ pending |
| 10-02-01 | 02 | 2 | OPTS-01, OPTS-02, OPTS-03, OPTS-04 | unit | `go test ./go/internal/sip/... -run TestOptions` | ✅ W0 | ⬜ pending |
| 10-02-02 | 02 | 2 | OPTS-04, OPTS-05 | race | `go test -race ./go/internal/sip/...` | ✅ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `go/internal/sip/registrar_test.go` — add stubs for `TestOptionsKeepalive_*` tests covering OPTS-01 through OPTS-04

*Note: Existing test infrastructure covers config and observability — no new test files needed for those packages.*

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| sipgate responds to unauthenticated OPTIONS | OPTS-02, OPTS-03 | Requires live sipgate credentials | Send OPTIONS to sipgate, verify response code and re-registration behavior |
| Goroutine stops cleanly on SIGTERM with active keepalive | OPTS-04 | Requires live process signal | Start service, SIGTERM, verify no goroutine leak via pprof or log output |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 5s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
