# Docker Image Size Budget

Operator runbook for the v3.0 6.0 MB Docker runtime image budget. Part
of the v3.0 hardening + release-gate documentation set. See
[RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) for the pre-merge
checklist that ties this runbook to the CI gate.

## Budget: 6.0 MB (6,000,000 bytes)

The runtime Docker image (multi-stage build, `FROM scratch` runtime
layer) MUST stay under 6,000,000 bytes when measured via `docker save
... | wc -c`. This is enforced as a build-blocking CI gate by `make
image-size-check`.

The integer 6,000,000 (decimal MB) is operator-readable rather than
binary-MiB-aligned ("image size: NNN bytes (limit: 6000000)").

## v3.0 budget rationale (6.0 MB)

On 2026-05-06 the built v3.0 image measured **5,466,112 bytes
(~5.2 MB)**. The 6,000,000-byte budget gives ~10 % margin against this
measurement while staying tight enough to catch material regressions
(a profiler addition or a new direct dependency >100 KB would still
flip the gate).

Forensic breakdown of the v3.0 measurement:

- Stripped, trimmed binary (`go build -ldflags="-s -w" -trimpath`):
  ~13.5 MB.
- OCI layer-blob in saved tarball: 5,323,680 bytes (~2.5×
  compression).
- Total saved tarball (binary layer + CA cert layer + manifests):
  5,466,112 bytes.

A static sipgo + gobwas/ws + pion/{rtp,srtp,sdp} +
prometheus/client_golang + zerolog Go binary cannot plausibly produce
a sub-1 MB OCI layer even with maximum stripping.

| Driver vs. v2.1                                                                      | Approximate size impact |
| ------------------------------------------------------------------------------------ | ----------------------- |
| Toolchain bump Go 1.24 → 1.26 (stdlib growth)                                        | +30-50 KB               |
| `github.com/emiago/sipgo` 1.2.0 → 1.3.0                                              | +10-20 KB               |
| `github.com/go-chi/chi/v5 v5.2.5` (REST router)                                      | +30-50 KB               |
| `golang.org/x/tools/go/packages` (lint-metrics binary; transitive into bridge build) | +50-100 KB              |
| REST API code, security middleware, status callback retrier, B2BUA forwarder         | +20-40 KB               |

## Verifying locally

```bash
cd /path/to/audio-dock
make image-size-check
```

Expected output (image fits):

```
docker buildx build --output type=image,push=false --load -t sipgate-bridge-sizecheck go/
... (build output) ...
image size: 5466112 bytes (limit: 6000000 bytes / ~5.7 MB)
```

Failure output (image exceeds budget):

```
image size: 6234567 bytes (limit: 6000000 bytes / ~5.7 MB)
FAIL: image exceeds 6.0 MB budget — see docs/operator/IMAGE_SIZE.md for re-budgeting procedure
make: *** [image-size-check] Error 1
```

### Manual recipe (without `make`)

```bash
cd go
docker buildx build --output type=image,push=false --load -t sipgate-bridge-sizecheck .
docker save sipgate-bridge-sizecheck | wc -c
# Result MUST be ≤ 6000000
```

The `--load` flag pulls the image into the local Docker daemon so
`docker save` can read it; without `--load`, buildx leaves the image
in the buildx cache only.

## CI verification

`.github/workflows/docker-go.yml` runs `make image-size-check` as a
build-blocking step on every push to `main` (and on every PR via
`workflow_dispatch` if invoked). A failed image-size-check blocks
merge.

## What triggers a re-budgeting review

Re-open the budget conversation if ANY of the following lands:

1. **A single new direct dependency exceeds 100 KB** of compiled binary
   contribution. Audit via:
   ```bash
   cd go && go build -ldflags="-s -w" -trimpath -o /tmp/bridge ./cmd/sipgate-sip-stream-bridge
   ls -l /tmp/bridge
   # Compare against the previous build size
   ```

2. **A toolchain bump (Go major or minor)** — even patch bumps
   occasionally grow stdlib by tens of KB.

3. **A net new feature** that requires a heavyweight library (e.g. a
   profiler, an OpenTelemetry exporter, a database driver). For
   feature-driven growth, account for image-size impact up front and
   either:
   - Reduce existing footprint elsewhere to stay under budget, OR
   - Propose a budget increase with a justification matching the
     driver-table format above.

4. **Sustained `make image-size-check` failures in CI** — if the
   margin shrinks below 50 KB and any small change can flip the gate,
   re-budget proactively rather than rolling back features.

## Why FROM scratch is the right choice

The runtime image is `FROM scratch` with only:

- The statically-linked Go binary (`-ldflags="-s -w" -trimpath`).
- `/etc/ssl/certs/ca-certificates.crt` from the Alpine builder stage
  (REQUIRED for `wss://` WebSocket TLS, status-callback HTTPS, and
  any `https://` TwiML fetch).

There is no shell, no libc, no package manager — minimal attack surface
+ minimal size. Any future move to a non-scratch base (e.g. `distroless`)
would need to re-justify both the size delta AND the attack-surface
trade-off.

## Re-budgeting procedure

If a justified re-budget is needed:

1. **Update CHANGELOG.md** under `## Changed` for the next release —
   record the new budget value with a brief justification (which
   dependency or toolchain bump prompted it).
2. **Update `Makefile` `image-size-check` target** — change the
   `LIMIT=6000000` constant to the new byte budget.
3. **Update `docs/operator/IMAGE_SIZE.md`** — update the budget table
   and the verification section.
4. **Communicate** to the operator team via the next release notes; a
   budget bump is operator-visible because it affects K8s image pull
   cost and disk pressure.

## Related runbooks

- [DASHBOARDS.md](./DASHBOARDS.md) — Grafana dashboards for runtime
  observability (orthogonal to image size, but the deploy artefact
  shape is comparable).
- [RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) — the
  `image-size-check` line in the operator pre-deploy checklist.
- [INCIDENT_RESPONSE.md](./INCIDENT_RESPONSE.md) — credential rotation
  (unrelated to image size; cross-link kept for runbook discoverability).

## Reference

- `go/Dockerfile` — multi-stage build, FROM scratch runtime
- `Makefile` `image-size-check` target
- `.github/workflows/docker-go.yml` — CI step running
  `make image-size-check`
- Docker buildx documentation: https://docs.docker.com/buildx/
- Go binary stripping flags reference: `go build -ldflags="-s -w"`
  (omits debug symbols + symbol table; `-trimpath` strips
  build-host paths)
