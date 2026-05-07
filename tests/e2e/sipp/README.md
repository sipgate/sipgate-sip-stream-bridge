# sipgate-sip-stream-bridge — sipp E2E Scenarios

Phase-18 ROADMAP SC #2 deliverable. Eight SIP-protocol-level scenario
scripts in [sipp](https://sipp.sourceforge.net/) XML format covering the
inbound-default-streaming + REST `<Dial>` + status-callback canonical
flows.

The Go-side e2e tests (CANCEL/BYE race, goroutine leak, privacy-gate stress,
shutdown ordering) live in `go/test/e2e/` (Plan 18-04). This directory
holds the SIP-protocol-level scenarios that exercise the bridge from
outside the process — the canonical operator-recognized SIP test artifact.

## Scenario Index

| Letter | File                                       | What it verifies                                                                                  |
| ------ | ------------------------------------------ | ------------------------------------------------------------------------------------------------- |
| a      | `inbound-default-streaming.xml`            | INVITE → 200 OK auto-answer → ACK → BYE → 200. Default streaming end-to-end.                      |
| b      | `inbound-rest-dial-answer.xml`             | INVITE → orchestrator REST `<Dial>` → outbound INVITE → 200 → dual-leg → BYE on inbound leg.      |
| c      | `inbound-rest-dial-busy.xml`               | Same as `b`; downstream returns 486 → DialCallStatus=busy → BYE w/ Reason cause=486.              |
| d      | `inbound-rest-dial-no-answer.xml`          | Same as `b`; downstream silent ≥30s → bridge CANCELs → 487 → BYE w/ DialCallStatus=no-answer.    |
| e      | `inbound-rest-dial-cancel.xml`             | Same as `b`; UAC BYEs inbound leg during dial-leg ringing → bridge tears down both legs.          |
| f      | `inbound-rest-dial-codec-mismatch.xml`     | Same as `b`; downstream answers G.722-only → bridge ACK + immediate BYE → DialCallStatus=failed (13224 / cause=49). |
| g      | `rest-hangup-mid-stream.xml`               | INVITE → 200 OK → orchestrator REST `Twiml=<Response><Hangup/>` → bridge BYE → 200.               |
| h      | `status-callback-flapping-host.xml`        | INVITE → 200 OK → BYE; flapping http stub returns 502/200/502/200; bridge retry policy succeeds.  |

The 8 filenames are LOCKED per `.planning/phases/18-hardening-graceful-
shutdown-release-gate/18-CONTEXT.md` "E2E Test Harness Format" section.
Do not rename.

## Running locally

### Prerequisites

- **sipp binary** (`sip-tester` apt package on Debian/Ubuntu, `sipp`
  Homebrew formula on macOS):
  ```bash
  apt-get install -y sip-tester    # Debian/Ubuntu
  brew install sipp                # macOS
  ```
- **Built bridge binary**:
  ```bash
  cd go && go build -o /tmp/sipgate-sip-stream-bridge ./cmd/sipgate-sip-stream-bridge
  ```
- **Built test-registrar stub** (responds to REGISTER → 200 OK so the
  bridge progresses past `registrar.Register()` at startup; not required
  when running against a real sipgate trunk):
  ```bash
  cd go && go build -o /tmp/test-registrar ./cmd/test-registrar
  ```
- **Python 3** for the inline stub WS server / flapping HTTP server.

### Invocation

From project root:

```bash
make e2e   # runs go/test/e2e/... + tests/e2e/sipp/run-sipp.sh
```

Or directly:

```bash
./tests/e2e/sipp/run-sipp.sh                            # all 8 scenarios
./tests/e2e/sipp/run-sipp.sh inbound-default-streaming  # scenario `a` only
./tests/e2e/sipp/run-sipp.sh --advisory-bh              # scenarios `b`-`h` only (CI advisory mode)
```

### Skip-pass behaviour

`run-sipp.sh` exits 0 (skip-pass, **not fail**) when:

- `sipp` is not on PATH — logs the install command and skips. CI installs
  `sip-tester` via apt explicitly, so this skip only fires for casual
  local invocation.
- The bridge binary cannot be built — logs the build command.
- UDP port 5060 cannot be bound (privileged-port permission on macOS, or
  port already in use). Workaround: `sudo` locally, or run inside docker
  with `-p 5060:5060/udp`.
- The architectural single-host single-port collision detected (see
  "Known limitation" below).

### Known limitation — single-host single-port collision

The bridge's `cmd/sipgate-sip-stream-bridge/main.go` hardcodes the SIP
listener to `0.0.0.0:5060`. The test-registrar stub also wants UDP 5060
to receive REGISTER from the bridge. On a single host without docker,
both processes collide.

Resolution paths:

1. **Operator walk against a real sipgate trunk** (Plan 18-06 Task 3) —
   the trunk acts as the registrar; no stub needed; bridge listener is
   the sole owner of UDP 5060. This is the canonical path for satisfying
   the build-blocking acceptance gate for scenario `a`.
2. **Docker container with port mapping** — the bridge runs inside the
   container on its own UDP 5060; test infrastructure runs on the host
   on different ports.
3. **Future plan: configurable listen port** — `SIP_LISTEN_PORT` env
   var (deferred — out-of-scope for Plan 18-03).

To override the gap-check skip when one of the above is in place:
`SKIP_GAP_CHECK=1 ./tests/e2e/sipp/run-sipp.sh`. The script will then
attempt to start the harness and run scenarios.

### Inspecting failures

`run-sipp.sh` writes per-scenario logs under `/tmp/sipp-e2e-$$/`
(printed at script-end so the operator can tail). Per-scenario logs:

- `<scenario>-msg.log` — sipp `--trace_msg` capture of every SIP message.
  Contains AUTH headers, raw SDP, full pcap-equivalent text. **Do NOT
  commit these logs** (they may carry session credentials).
- `<scenario>-err.log` — sipp's stderr / scenario-error trace.
- `bridge.log` — the bridge under test's stdout/stderr (zerolog JSON).
- `stub-ws.log` — Python stub WebSocket server log.
- `test-registrar.log` — test-registrar stub log.

To enable pcap capture, run `tcpdump` on the host externally:

```bash
sudo tcpdump -i lo0 -w /tmp/phase18-e2e.pcap 'udp port 5060 or udp portrange 10000-10099'
```

## CI integration

`.github/workflows/docker-go.yml` installs `sip-tester` and runs
`make e2e` (build-blocking for scenario `a`) and
`tests/e2e/sipp/run-sipp.sh --advisory-bh` (advisory for scenarios
`b`-`h`, `continue-on-error: true`).

Per Plan 18-06 Task 3, the operator walk against a real sipgate trunk
gates scenarios `b`-`h` to operator sign-off; intractable scenarios
trigger return-to-Plan-18-03 iteration.

## Adding a new scenario

1. Add the XML under `tests/e2e/sipp/`. Use one of the existing files
   as a starting point; the sipp DTD reference is at
   <https://sipp.sourceforge.net/doc/reference.html>.
2. Add the filename to the `SCENARIOS` array in `run-sipp.sh` (preserve
   ordering; scenario `a` MUST stay first because the build-blocking
   gate iterates positionally).
3. Document the scenario in the index table above with its ROADMAP
   letter and one-line behavior summary.
4. If the scenario needs a special downstream UAS / orchestrator
   stub, add it to `run-sipp.sh` and document the orchestration in the
   scenario's first comment block.
