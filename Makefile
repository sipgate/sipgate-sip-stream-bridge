# Makefile — sipgate audio-dock
#
# Convenience entry points for the Go module under ./go. The Go test/build
# is the source of truth; this file primarily wires the cardinality lint
# binary (cmd/lint-metrics) and the e2e harness into single targets that
# CI invokes as build-blocking steps.
#
# Usage:
#   make test             — full Go test suite under -race -count=3
#   make lint             — golangci-lint on the Go module
#   make lint-metrics     — cmd/lint-metrics on every Go package
#                           (cardinality + secret-mask discipline)
#   make build            — go build ./... under the Go module
#   make e2e              — Go integration tests + 8 sipp scenarios
#   make image-size-check — Docker image budget enforcer (≤6.0 MB)
#   make all              — test + lint + lint-metrics + build
#
# CI integration: .github/workflows/docker-go.yml invokes the same
# targets so local development and CI behave identically.

.PHONY: all test lint lint-metrics build e2e image-size-check

all: test lint lint-metrics build

test:
	cd go && go test -race -count=3 ./...

lint:
	cd go && golangci-lint run

# lint-metrics — cardinality + secret-mask discipline AST walker.
# Validates every prometheus *Vec.WithLabelValues call site against the
# // metrics:allowlist comments adjacent to each *Vec declaration AND
# every zerolog Info+ Str("from"|"to"|"url"|...) emit against the
# phone-number / URL debug-only convention. Exits 0 on success, 1 on
# violations (with file:line diagnostics on stderr), 2 on packages.Load
# error.
lint-metrics:
	cd go && go run ./cmd/lint-metrics ./...

build:
	cd go && go build ./...

# e2e — end-to-end scenario suite. Runs the Go integration tests at
# go/test/e2e/ plus all 8 sipp scenarios under tests/e2e/sipp/ against
# a co-hosted test-registrar stub, stub WS server, flapping HTTP stub
# (for the operator-trusted default StatusCallback), and per-scenario
# sipp UAS stubs (run-sipp.sh wires every component).
#
# Build-blocking discipline: all Go tests + all 8 sipp scenarios must
# exit 0. SIP_LISTEN_ADDR + SIP_OUTBOUND_TARGET_PORT keep the bridge,
# registrar stub, and UAS stubs co-existent on a single host.
# Requires the sipp binary (sip-tester apt package); CI installs it
# explicitly so skip-pass only fires for casual local invocation.
e2e:
	cd go && go test -race -count=1 ./test/e2e/...
	bash tests/e2e/sipp/run-sipp.sh inbound-default-streaming
	bash tests/e2e/sipp/run-sipp.sh inbound-rest-dial-answer
	bash tests/e2e/sipp/run-sipp.sh inbound-rest-dial-busy
	bash tests/e2e/sipp/run-sipp.sh inbound-rest-dial-no-answer
	bash tests/e2e/sipp/run-sipp.sh inbound-rest-dial-cancel
	bash tests/e2e/sipp/run-sipp.sh inbound-rest-dial-codec-mismatch
	bash tests/e2e/sipp/run-sipp.sh rest-hangup-mid-stream
	bash tests/e2e/sipp/run-sipp.sh status-callback-flapping-host

# image-size-check — runtime Docker image budget enforcer (≤6.0 MB).
# Builds the multi-stage `FROM scratch` runtime image and asserts the
# saved tarball stays under 6,000,000 bytes. The 6 MB limit gives ~10 %
# margin against the measured ~5.5 MB baseline while still catching
# material regressions (a profiler addition or new direct dependency
# >100 KB would flip the gate). A static sipgo + gobwas/ws +
# pion/{rtp,srtp,sdp} + prometheus/client_golang + zerolog Go binary
# cannot plausibly fit under 1 MB even with `-ldflags="-s -w" -trimpath`.
#
# Any future regression — single new direct dependency >100 KB or
# toolchain bump — MUST trigger a re-budgeting review documented at
# docs/operator/IMAGE_SIZE.md.
#
# Local-equivalent recipe (without buildx): `docker build -t bridge
# go/` then `docker save bridge | wc -c`. The Makefile target uses
# buildx for CI parity with the docker/build-push-action workflow step.
image-size-check:
	docker buildx build --output type=image,push=false --load -t sipgate-bridge-sizecheck go/
	@SIZE=$$(docker save sipgate-bridge-sizecheck | wc -c); \
	 LIMIT=6000000; \
	 echo "image size: $$SIZE bytes (limit: $$LIMIT bytes / ~5.7 MB)"; \
	 if [ $$SIZE -gt $$LIMIT ]; then \
	   echo "FAIL: image exceeds 6.0 MB budget — see docs/operator/IMAGE_SIZE.md for re-budgeting procedure"; \
	   exit 1; \
	 fi
