---
phase: 04-go-scaffold
plan: 02
subsystem: infra
tags: [docker, go, static-binary, from-scratch, ca-certs, docker-compose]

# Dependency graph
requires:
  - phase: 04-01
    provides: go module github.com/sipgate/sipgate-sip-stream-bridge with cmd/sipgate-sip-stream-bridge entry point
provides:
  - Docker image sipgate-sip-stream-bridge:latest at ~1.06 MB (FROM scratch, statically-linked Go binary)
  - Dockerfile: two-stage golang:1.26-alpine builder + FROM scratch runtime
  - CA certificates at /etc/ssl/certs/ca-certificates.crt in final image (Phase 6 TLS readiness)
  - docker-compose.yml with network_mode: host and macOS override documentation
affects: [05-sip-ua, 06-bridge, 07-http, 08-lifecycle]

# Tech tracking
tech-stack:
  added:
    - golang:1.26-alpine (Docker builder image)
    - FROM scratch (Docker runtime layer)
  patterns:
    - "Docker build: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 for static cross-compile"
    - "Docker size: -ldflags='-s -w' + -trimpath strips symbols and local paths"
    - "Docker compose: network_mode: host required for RTP on Linux; macOS uses bridge override"
    - "Docker CA certs: COPY --from=builder /etc/ssl/certs/ca-certificates.crt from alpine builder"

key-files:
  created: []
  modified:
    - Dockerfile
    - docker-compose.yml

key-decisions:
  - "GOARCH=amd64 explicitly set in Dockerfile even on ARM build hosts — avoids exec format errors in Linux CI/prod"
  - "CA certs copied in this phase not Phase 6 — prevents Dockerfile layer invalidation when TLS is added"
  - "No RTP EXPOSE range in Dockerfile — large ranges stall Docker Desktop port proxy on macOS (Phase 6 adds with warning)"

patterns-established:
  - "Docker layer order: COPY go.mod go.sum + RUN go mod download before COPY . . — dep cache layer only invalidates on lockfile change"
  - "FROM scratch pattern: copy only binary + CA certs; no shell, no libc, minimal attack surface"

requirements-completed: [DCK-01, DCK-02, DCK-03]

# Metrics
duration: 2min
completed: 2026-03-03
---

# Phase 4 Plan 02: Docker Build Summary

**FROM scratch two-stage Go Docker image at 1.06 MB with CA certs for Phase 6 TLS, replacing Node.js multi-stage build**

## Performance

- **Duration:** ~2 min
- **Started:** 2026-03-03T22:04:12Z
- **Completed:** 2026-03-03T22:06:15Z
- **Tasks:** 2
- **Files modified:** 2

## Accomplishments

- Dockerfile replaced: Node.js 22 pnpm multi-stage build fully superseded by Go two-stage static build
- Final image size: 1.06 MB (well under 20 MB DCK-03 target); FROM scratch with no shell, no libc
- CA certificates copied from alpine builder into final image — Phase 6 wss:// TLS connections work without Dockerfile changes
- docker-compose.yml updated: v2.0 comments, Phase 6 RTP context, macOS override with SIP port mapping and UDP range warning

## Task Commits

Each task was committed atomically:

1. **Task 1: Replace Dockerfile with Go two-stage static build** - `531461f` (feat)
2. **Task 2: Update docker-compose.yml for Go binary** - `a23a2e5` (chore)

## Files Created/Modified

- `Dockerfile` - Two-stage build: golang:1.26-alpine builder produces static binary; FROM scratch runtime with binary + CA certs
- `docker-compose.yml` - Updated v2.0 header comment; macOS bridge override example with SIP ports and 100-port UDP range warning

## Decisions Made

- GOARCH=amd64 explicitly set even on ARM Mac hosts — prevents exec format error when deploying to Linux x86_64 CI/production
- CA certs included now (Phase 4) not deferred to Phase 6 — avoids invalidating a Docker layer cache when TLS is added
- No RTP port range EXPOSE in Dockerfile — large ranges cause Docker Desktop port proxy to stall on macOS startup; Phase 6 adds with explicit macOS override documentation

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None — docker build succeeded on first attempt, image size 1.06 MB (plan target <20 MB), all verification checks passed.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness

- Phase 5 (SIP UA): can build and run via `docker build -t sipgate-sip-stream-bridge:latest . && docker run --rm -e ... sipgate-sip-stream-bridge:latest`
- Phase 6 (Bridge/RTP): Dockerfile already has CA certs — no changes needed to support wss:// connections
- CI/CD: `docker build -t sipgate-sip-stream-bridge:latest .` is the full build command; no Node.js, no pnpm, no transpile step

---
*Phase: 04-go-scaffold*
*Completed: 2026-03-03*

## Self-Check: PASSED

- FOUND: Dockerfile
- FOUND: docker-compose.yml
- FOUND: .planning/phases/04-go-scaffold/04-02-SUMMARY.md
- FOUND commit: 531461f (feat(04-02): replace Node.js Dockerfile with Go two-stage static build)
- FOUND commit: a23a2e5 (chore(04-02): update docker-compose.yml comments for v2.0 Go binary)
- VERIFIED: FROM scratch in Dockerfile
- VERIFIED: ca-certificates.crt COPY in Dockerfile
- VERIFIED: CGO_ENABLED=0 in Dockerfile
- VERIFIED: network_mode: host in docker-compose.yml
- VERIFIED: Image size 1.06 MB (< 20 MB target)
