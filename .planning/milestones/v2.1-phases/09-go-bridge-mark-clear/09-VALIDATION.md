---
phase: 9
slug: go-bridge-mark-clear
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-05
---

# Phase 9 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | go test |
| **Config file** | none — standard Go test toolchain |
| **Quick run command** | `go test ./go/internal/bridge/...` |
| **Full suite command** | `go test -race ./go/internal/bridge/... ./go/internal/observability/...` |
| **Estimated runtime** | ~5 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./go/internal/bridge/...`
- **After every plan wave:** Run `go test -race ./go/internal/bridge/... ./go/internal/observability/...`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** ~5 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 9-01-01 | 01 | 1 | MARK-01, MARK-02, MARK-03, MARK-04 | unit | `go test ./go/internal/bridge/...` | ✅ | ⬜ pending |
| 9-01-02 | 01 | 1 | MARK-04 | race | `go test -race ./go/internal/bridge/...` | ✅ | ⬜ pending |
| 9-02-01 | 02 | 2 | MARK-01 | unit | `go test ./go/internal/bridge/... -run TestMark` | ✅ W0 | ⬜ pending |
| 9-02-02 | 02 | 2 | MARK-02 | unit | `go test ./go/internal/bridge/... -run TestMarkImmediate` | ✅ W0 | ⬜ pending |
| 9-02-03 | 02 | 2 | MARK-03 | unit | `go test ./go/internal/bridge/... -run TestClear` | ✅ W0 | ⬜ pending |
| 9-03-01 | 03 | 2 | MARK-01, MARK-02, MARK-03, MARK-04 | unit+race | `go test -race ./go/internal/bridge/... ./go/internal/observability/...` | ✅ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `go/internal/bridge/ws_test.go` — add `TestSendMarkEcho_JSONSchema` stub
- [ ] `go/internal/bridge/session_test.go` (if applicable) — stubs for MARK-01, MARK-02, MARK-03 channel logic tests

*Note: `go test -race` (MARK-04) uses the existing test runner — no Wave 0 infrastructure needed beyond the test stubs.*

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| End-to-end barge-in with real Twilio call | MARK-01, MARK-02, MARK-03 | Requires live SIP + Twilio session | Place test call, send TTS, trigger clear from WS server side, verify caller hears silence and mark echo arrives |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 5s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
