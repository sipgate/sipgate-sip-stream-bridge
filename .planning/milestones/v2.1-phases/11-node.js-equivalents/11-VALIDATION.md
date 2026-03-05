---
phase: 11
slug: node-js-equivalents
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-05
---

# Phase 11 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Vitest (installed in Wave 0) |
| **Config file** | none — `vitest run` auto-discovers `node/test/**/*.test.ts` |
| **Quick run command** | `cd node && pnpm test` |
| **Full suite command** | `cd node && pnpm test` |
| **Estimated runtime** | ~5 seconds |

---

## Sampling Rate

- **After every task commit:** Run `cd node && pnpm test`
- **After every plan wave:** Run `cd node && pnpm test`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** ~5 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 11-01-01 | 01 | 0 | MRKN-01, MRKN-02, MRKN-03 | unit (stubs) | `cd node && pnpm test` | ❌ W0 | ⬜ pending |
| 11-01-02 | 01 | 0 | OPTN-01, OPTN-02, OPTN-03 | unit (stubs) | `cd node && pnpm test` | ❌ W0 | ⬜ pending |
| 11-02-01 | 02 | 1 | MRKN-01, MRKN-02 | unit | `cd node && pnpm test` | ❌ W0 | ⬜ pending |
| 11-02-02 | 02 | 1 | MRKN-03 | unit | `cd node && pnpm test` | ❌ W0 | ⬜ pending |
| 11-03-01 | 03 | 2 | OPTN-01, OPTN-02, OPTN-03 | unit | `cd node && pnpm test` | ❌ W0 | ⬜ pending |
| 11-03-02 | 03 | 2 | OPTN-01, OPTN-02, OPTN-03 | unit | `cd node && pnpm test` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `node/test/wsClient.mark.test.ts` — failing stubs for MRKN-01, MRKN-02, MRKN-03
- [ ] `node/test/userAgent.options.test.ts` — failing stubs for OPTN-01, OPTN-02, OPTN-03
- [ ] `node/package.json` — add `"test": "vitest run"` script
- [ ] Vitest install: `cd node && pnpm add -D vitest`

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| Live SIP OPTIONS to sipgate returns 200 OK | OPTN-01 | Requires live sipgate UDP endpoint | Start service, monitor logs for `options_ok` debug entries at 30s intervals |
| Mark echo reaches WS backend after audio drain | MRKN-01 | Requires live WS backend session | Send mark from backend, verify echo arrives after audio plays out |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 5s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
