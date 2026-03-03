---
phase: 01-foundation
plan: 03
subsystem: infra
tags: [docker, dockerfile, pnpm, node22-alpine, multi-stage-build, docker-compose, env]

# Dependency graph
requires:
  - phase: 01-01
    provides: "pnpm-lock.yaml, package.json, dist/index.js build output (via tsup), src/index.ts entrypoint"
provides:
  - "Multi-stage Dockerfile: base/fetcher/builder/production on node:22-alpine with pnpm fetch layer caching"
  - "docker-compose.yml with network_mode: host for Linux production RTP and env_file: .env"
  - ".env.example documenting all 10 env vars (5 required, 5 optional-with-defaults)"
affects: [02-sip, 03-media, all-phases, ops]

# Tech tracking
tech-stack:
  added:
    - "Docker multi-stage build (node:22-alpine)"
    - "pnpm fetch layer-cache pattern"
    - "docker-compose v2 with network_mode: host"
  patterns:
    - "pnpm fetch in fetcher stage — cache invalidates only on pnpm-lock.yaml change, not source changes"
    - "COPY --from=fetcher store → production offline install — no network at prod install time"
    - "network_mode: host for Linux RTP — avoids Docker port-proxy UDP jitter"

key-files:
  created:
    - "Dockerfile"
    - "docker-compose.yml"
    - ".env.example"
  modified: []

key-decisions:
  - "4-stage Dockerfile (base/fetcher/builder/production) — clean separation of concerns; fetcher stage is the only one touching the network for pnpm packages"
  - "pnpm fetch lockfile layer before COPY source — layer only invalidates when pnpm-lock.yaml changes, not on TypeScript edits"
  - "network_mode: host in docker-compose.yml — required for RTP UDP in Phase 2 (Docker port-proxy adds ~10ms per-packet jitter)"
  - "EXPOSE 10000-10099/udp with RTP comment block — forward-looking operational note for Phase 2 operators"
  - "MEDIUM confidence warning on SIP_DOMAIN and SIP_REGISTRAR — values need verification from sipgate portal"

patterns-established:
  - "Multi-stage Docker build: fetcher caches deps, builder compiles, production ships minimal image"
  - ".env.example committed; .env gitignored — operators copy and fill in credentials"

requirements-completed: [DCK-01, DCK-02, DCK-03]

# Metrics
duration: 2min
completed: 2026-03-03
---

# Phase 1 Plan 03: Docker Infrastructure Summary

**Multi-stage node:22-alpine Dockerfile with pnpm fetch layer caching, docker-compose.yml with network_mode: host for RTP, and .env.example documenting all 10 env vars**

## Performance

- **Duration:** 2 min
- **Started:** 2026-03-03T10:15:16Z
- **Completed:** 2026-03-03T10:17:03Z
- **Tasks:** 2
- **Files modified:** 3 (created)

## Accomplishments
- 4-stage Dockerfile (base/fetcher/builder/production) on node:22-alpine; pnpm fetch layer invalidates only on lockfile changes; production stage installs --prod --offline from fetcher store; runs as non-root USER node
- docker-compose.yml with network_mode: host (required for Phase 2 RTP — avoids Docker port-proxy UDP jitter), env_file: .env, restart: unless-stopped
- .env.example with all 10 env vars: 5 required (SIP_USER, SIP_PASSWORD, SIP_DOMAIN, SIP_REGISTRAR, WS_TARGET_URL) + 5 optional with defaults (SDP_CONTACT_IP, RTP_PORT_MIN, RTP_PORT_MAX, SIP_EXPIRES, LOG_LEVEL); MEDIUM confidence warnings on sipgate endpoint URLs

## Task Commits

Each task was committed atomically:

1. **Task 1: Dockerfile — multi-stage node:22-alpine with pnpm fetch layer caching** - `ece1ab3` (feat)
2. **Task 2: docker-compose.yml and .env.example** - `e19564a` (feat)

**Plan metadata:** (docs commit follows)

## Files Created/Modified
- `Dockerfile` - 4-stage multi-stage build: base (corepack+pnpm) / fetcher (pnpm fetch lockfile cache) / builder (offline install + tsup compile) / production (node:22-alpine, --prod --offline, non-root user node, RTP comment, EXPOSE 10000-10099/udp, CMD node dist/index.js)
- `docker-compose.yml` - Linux production config: network_mode: host, env_file: .env, restart: unless-stopped; includes macOS override instructions in comments
- `.env.example` - Documents all 10 env vars; required vars have empty values; optional vars commented out with defaults; MEDIUM confidence notes on sipgate endpoint URLs

## Decisions Made
- Used 4-stage Dockerfile (not 3) to cleanly separate the corepack/pnpm activation (base) from the lockfile fetch (fetcher) — base stage is shared by both fetcher and builder
- pnpm store path `/root/.local/share/pnpm/store` confirmed at build time — this is the default for node:22-alpine running as root; COPY --from=fetcher transfers it to production stage for offline prod install
- RTP comment block placed before EXPOSE 10000-10099/udp — documents Phase 2 networking requirements for operators
- network_mode: host in docker-compose.yml — Docker's NAT port-proxy adds ~10ms UDP jitter per packet which would degrade RTP audio quality; host networking eliminates this

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
None.

## User Setup Required
None for Plan 03 itself. However, before using docker compose up, operators must:
1. Copy `.env.example` to `.env`
2. Fill in real sipgate credentials (SIP_USER, SIP_PASSWORD)
3. Verify SIP_DOMAIN and SIP_REGISTRAR from the sipgate portal (MEDIUM confidence values provided as defaults)

## Next Phase Readiness
- Docker infrastructure is complete — the service can be built and shipped as a container
- `docker build .` produces a working image; container exits code 1 with JSON config error if env vars missing (verified)
- Phase 2 (SIP transport + RTP media) can be containerized using this Dockerfile without changes
- macOS developers will need docker-compose.override.yml to publish UDP ports for Phase 2 RTP testing (instructions in docker-compose.yml comments)

---
*Phase: 01-foundation*
*Completed: 2026-03-03*

## Self-Check: PASSED

- FOUND: Dockerfile
- FOUND: docker-compose.yml
- FOUND: .env.example
- FOUND: .planning/phases/01-foundation/01-03-SUMMARY.md
- FOUND commit: ece1ab3 (Task 1 — Dockerfile)
- FOUND commit: e19564a (Task 2 — docker-compose.yml and .env.example)
