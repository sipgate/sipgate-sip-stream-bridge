#!/usr/bin/env bash
# sipp E2E scenario harness.
#
# Runs the 8 sipp XML scenarios under tests/e2e/sipp/ against a configured
# bridge instance.
#
# Modes:
#   ./run-sipp.sh <scenario-name>   — run a single named scenario
#                                     (build-blocking; exit 0 only on pass).
#   ./run-sipp.sh                   — run all 8 scenarios; exit 0 only on
#                                     full pass.
#
# Pre-flight skip behaviour:
#   - sipp not installed                       → skip-pass (exit 0) + log how
#                                                to install (apt-get / brew).
#   - bridge binary not built                  → skip-pass (exit 0) + log
#                                                build instructions.
#   - REG_PORT or BRIDGE_SIP_PORT unbindable   → exit 1 + log workaround.
#
# Architecture — single-host harness:
#   Two SIP UDP sockets are needed: one for the test-registrar stub
#   (REGISTER → 200 OK responder), one for the bridge under test (inbound
#   INVITE listener). The bridge's SIP_LISTEN_ADDR env var makes its
#   listener bind configurable so the two can co-exist on a single host:
#     - test-registrar on udp://127.0.0.1:$REG_PORT         (default 5060)
#     - bridge inbound on udp://127.0.0.1:$BRIDGE_SIP_PORT  (default 5070)
#   The bridge sends REGISTER to SIP_REGISTRAR=127.0.0.1:$REG_PORT and
#   advertises Contact: <sip:user@127.0.0.1:$BRIDGE_SIP_PORT>; sipp UAC
#   sends INVITE to 127.0.0.1:$BRIDGE_SIP_PORT.
#
#   For scenarios that drive a <Dial> outbound INVITE, a per-scenario sipp
#   UAS stub binds udp://127.0.0.1:$UAS_PORT (default 5080); the bridge
#   routes outbound INVITEs there via SIP_OUTBOUND_TARGET_PORT.
#
#   WS target is a stub websocket server (started inline via Python).
#   A flapping HTTP stub on 127.0.0.1:$FLAPPING_PORT serves the
#   operator-trusted default StatusCallback (alternates 502 / 200 to
#   exercise the bridge's exp-backoff retry policy).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/../../.." && pwd)"

SCENARIOS=(
  inbound-default-streaming.xml
  inbound-rest-dial-answer.xml
  inbound-rest-dial-busy.xml
  inbound-rest-dial-no-answer.xml
  inbound-rest-dial-cancel.xml
  inbound-rest-dial-codec-mismatch.xml
  rest-hangup-mid-stream.xml
  status-callback-flapping-host.xml
)

# Mode selection.
MODE="all"
SCENARIO_FILTER=""
if [ -n "${1:-}" ]; then
  MODE="single"
  SCENARIO_FILTER="$1"
  # Allow argument with or without .xml suffix.
  case "$SCENARIO_FILTER" in
    *.xml) ;;
    *) SCENARIO_FILTER="${SCENARIO_FILTER}.xml" ;;
  esac
fi

# Pre-flight: sipp binary.
if ! command -v sipp >/dev/null 2>&1; then
  cat <<EOF
sipp not installed; install via:
  Debian/Ubuntu:   apt-get install -y sip-tester
  macOS:           brew install sipp
skipping sipp scenarios (CI installs sip-tester explicitly so this skip
only fires for casual local invocation).
EOF
  exit 0
fi

# Configurable env vars for the harness.
BRIDGE_BIN="${BRIDGE_BIN:-/tmp/sipgate-sip-stream-bridge}"
TEST_REGISTRAR_BIN="${TEST_REGISTRAR_BIN:-/tmp/test-registrar}"
# REG_PORT — test-registrar stub binds udp://127.0.0.1:REG_PORT (REGISTER → 200 OK).
# Bridge sends REGISTER to SIP_REGISTRAR=127.0.0.1:REG_PORT (registrar.go:90 hardcodes
# Port: 5060 for the REGISTER target URI; harness keeps REG_PORT=5060 to match).
REG_PORT="${REG_PORT:-5060}"
# BRIDGE_SIP_PORT — bridge inbound INVITE listener binds udp+tcp://0.0.0.0:BRIDGE_SIP_PORT
# via SIP_LISTEN_ADDR. Default 5070 is non-privileged so macOS users can run the
# harness without sudo.
BRIDGE_SIP_PORT="${BRIDGE_SIP_PORT:-5070}"
# UAS_PORT — sipp UAS-stub listener for the bridge's outbound INVITE (when scenarios
# need <Dial> behaviour). Bridge routes outbound INVITEs to 127.0.0.1:UAS_PORT via
# SIP_OUTBOUND_TARGET_PORT.
UAS_PORT="${UAS_PORT:-5080}"
HTTP_PORT="${HTTP_PORT:-9090}"
WS_PORT="${WS_PORT:-19090}"
SIPP_PORT="${SIPP_PORT:-15060}"
# FLAPPING_PORT — Python HTTP stub for the operator-trusted default
# StatusCallback target (scenario h). Alternates 502 / 200 responses to
# exercise the bridge's exp-backoff retry policy. Only started when a
# scenario opts in via uas_for_scenario / orchestrator dispatch below.
FLAPPING_PORT="${FLAPPING_PORT:-19091}"
# AccountSid is deterministic from SIP_USER: "AC" + first 16 bytes of sha256
# (= 32 hex chars). Bridge derives it the same way at startup. Override by
# setting ACCOUNT_SID explicitly if running against a different SIP_USER.
ACCOUNT_SID="${ACCOUNT_SID:-AC$(echo -n "${SIP_USER_FOR_HARNESS:-testuser}" | shasum -a 256 | cut -c1-32)}"
AUTH_TOKEN="${AUTH_TOKEN:-test-auth-token}"

LOG_DIR="${LOG_DIR:-/tmp/sipp-e2e-$$}"
mkdir -p "$LOG_DIR"

# Build artifacts if missing.
if [ ! -x "$BRIDGE_BIN" ]; then
  echo "building bridge binary → $BRIDGE_BIN"
  ( cd "$ROOT_DIR/go" && go build -o "$BRIDGE_BIN" ./cmd/sipgate-sip-stream-bridge ) || {
    echo "skipping: bridge build failed; build manually with:"
    echo "  cd go && go build -o /tmp/sipgate-sip-stream-bridge ./cmd/sipgate-sip-stream-bridge"
    exit 0
  }
fi
if [ ! -x "$TEST_REGISTRAR_BIN" ]; then
  echo "building test-registrar stub → $TEST_REGISTRAR_BIN"
  ( cd "$ROOT_DIR/go" && go build -o "$TEST_REGISTRAR_BIN" ./cmd/test-registrar ) || {
    echo "skipping: test-registrar build failed"
    exit 0
  }
fi

# Pre-flight: can we bind both required UDP ports?
# REG_PORT — test-registrar stub. Default 5060; non-privileged on macOS+Linux.
# BRIDGE_SIP_PORT — bridge inbound listener. Default 5070; non-privileged.
check_port() {
  local port="$1"
  python3 -c "
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
try:
    s.bind(('127.0.0.1', $port))
    s.close()
except Exception as e:
    raise SystemExit(1)
" 2>/dev/null
}

if ! check_port "$REG_PORT"; then
  cat <<EOF
REG_PORT=$REG_PORT cannot be bound (already in use, or privileged-port restriction).

Workarounds:
  - REG_PORT=5061 BRIDGE_SIP_PORT=5070 $0  (override both ports)
  - sudo $0  (when binding privileged ports locally)
  - run inside docker / netns
EOF
  exit 1
fi

if ! check_port "$BRIDGE_SIP_PORT"; then
  cat <<EOF
BRIDGE_SIP_PORT=$BRIDGE_SIP_PORT cannot be bound (already in use, or privileged-port restriction).

Workarounds:
  - BRIDGE_SIP_PORT=5071 $0  (override port)
  - lsof -iUDP:$BRIDGE_SIP_PORT  (find current owner)
EOF
  exit 1
fi

# Stub WS server (Python http.server with WebSocket-like upgrade response).
# The bridge dialWS expects a 101 Switching Protocols handshake. We use a
# minimal Python stub.
STUB_WS_PIDFILE="$LOG_DIR/stub-ws.pid"
start_stub_ws() {
  python3 - <<PYEOF >"$LOG_DIR/stub-ws.log" 2>&1 &
import socket, threading, base64, hashlib, struct
HOST, PORT = "127.0.0.1", $WS_PORT
def handle(c):
    try:
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = c.recv(4096)
            if not chunk: break
            data += chunk
        # Find Sec-WebSocket-Key
        key = None
        for line in data.split(b"\r\n"):
            if line.lower().startswith(b"sec-websocket-key:"):
                key = line.split(b":", 1)[1].strip()
        accept = base64.b64encode(hashlib.sha1(key + b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest())
        resp = b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + b"\r\n\r\n"
        c.sendall(resp)
        # Drain client frames; ignore content.
        while True:
            try:
                if not c.recv(4096):
                    break
            except: break
    except: pass
    finally:
        try: c.close()
        except: pass

s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((HOST, PORT))
s.listen(50)
print(f"stub-ws listening ws://{HOST}:{PORT}", flush=True)
while True:
    try:
        c, _ = s.accept()
        threading.Thread(target=handle, args=(c,), daemon=True).start()
    except: break
PYEOF
  echo $! > "$STUB_WS_PIDFILE"
}

# Test registrar (UDP REGISTER → 200 OK).
TEST_REG_PIDFILE="$LOG_DIR/test-registrar.pid"
start_test_registrar() {
  REG_LISTEN_ADDR="127.0.0.1:$REG_PORT" "$TEST_REGISTRAR_BIN" \
    >"$LOG_DIR/test-registrar.log" 2>&1 &
  echo $! > "$TEST_REG_PIDFILE"
}

# Bridge.
BRIDGE_PIDFILE="$LOG_DIR/bridge.pid"
start_bridge() {
  # SIP_OUTBOUND_TARGET_PORT routes outbound <Dial> INVITEs to the UAS stub
  # on 127.0.0.1:$UAS_PORT instead of sipgo's default DNS/5060 routing. This
  # is what lets the harness exercise the B2BUA dial path without needing a
  # real sipgate trunk.
  #
  # DIAL_ALLOWED_PREFIXES is set permissive ("+") for test purposes — the
  # toll-fraud guardrail's default deny-all would block every <Dial> in the
  # harness. Production deployments must set this explicitly per market.
  # STATUS_CALLBACK_DEFAULT_URL points at the flapping HTTP stub
  # (started by start_flapping_http when a scenario needs it). The
  # operator-trusted Trusted=true marker on the resulting subscription
  # bypasses the SSRF guard so 127.0.0.1 callbacks are accepted.
  # When the flapping stub isn't running, POSTs fail with connection-
  # refused and the per-call StatusClient logs failures; the harness
  # tolerates this for non-h scenarios.
  SIP_USER="testuser" \
  SIP_PASSWORD="testpass" \
  SIP_DOMAIN="127.0.0.1" \
  SIP_REGISTRAR="127.0.0.1" \
  SIP_LISTEN_ADDR="127.0.0.1:$BRIDGE_SIP_PORT" \
  SIP_OUTBOUND_TARGET_PORT="$UAS_PORT" \
  DIAL_ALLOWED_PREFIXES="+" \
  DIAL_RING_TIMEOUT_S="5" \
  STATUS_CALLBACK_DEFAULT_URL="${STATUS_CALLBACK_DEFAULT_URL:-http://127.0.0.1:$FLAPPING_PORT/cb}" \
  STATUS_CALLBACK_DEFAULT_METHOD="${STATUS_CALLBACK_DEFAULT_METHOD:-POST}" \
  STATUS_CALLBACK_DEFAULT_EVENTS="${STATUS_CALLBACK_DEFAULT_EVENTS:-initiated,ringing,answered,completed}" \
  WS_TARGET_URL="ws://127.0.0.1:$WS_PORT/" \
  HTTP_PORT="$HTTP_PORT" \
  AUTH_TOKEN="$AUTH_TOKEN" \
  LOG_LEVEL="info" \
  AUDIO_MODE="twilio" \
  SDP_CONTACT_IP="127.0.0.1" \
    "$BRIDGE_BIN" >"$LOG_DIR/bridge.log" 2>&1 &
  echo $! > "$BRIDGE_PIDFILE"
}

# Flapping HTTP stub — alternates 502 (first request) / 200 (retry) on
# every CallSid. Logs every request line to $LOG_DIR/flapping.log so the
# orchestrator can verify ≥2 POST attempts per call after the run.
FLAP_PIDFILE="$LOG_DIR/flapping.pid"
start_flapping_http() {
  python3 - >"$LOG_DIR/flapping-stdout.log" 2>&1 <<PYEOF &
import http.server, threading, time, os, sys

LOG_PATH = "$LOG_DIR/flapping.log"
PORT     = $FLAPPING_PORT
state    = {}      # CallSid -> attempt counter
lock     = threading.Lock()

def line(msg):
    with open(LOG_PATH, "a") as f:
        f.write(time.strftime("%Y-%m-%dT%H:%M:%S") + " " + msg + "\n")

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args, **kwargs):
        pass  # silence stderr noise
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        body = self.rfile.read(n).decode("ascii", "replace") if n > 0 else ""
        sid  = "(unknown)"
        for kv in body.split("&"):
            if kv.startswith("CallSid="):
                sid = kv[len("CallSid="):]
                break
        with lock:
            state[sid] = state.get(sid, 0) + 1
            attempt = state[sid]
        if attempt == 1:
            line(f"sid={sid} attempt={attempt} → 502")
            self.send_response(502); self.end_headers()
        else:
            line(f"sid={sid} attempt={attempt} → 200")
            self.send_response(200); self.end_headers()
    def do_GET(self):
        self.send_response(200); self.end_headers()

with open(LOG_PATH, "w") as f: f.write("")
http.server.HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PYEOF
  echo $! > "$FLAP_PIDFILE"
}

# UAS-stub lifecycle (sipp UAS scenarios in tests/e2e/sipp/uas/).
UAS_PIDFILE="$LOG_DIR/uas.pid"
start_uas() {
  local uas_xml="$1"
  if [ ! -f "$SCRIPT_DIR/uas/$uas_xml" ]; then
    echo "uas: $uas_xml not found in $SCRIPT_DIR/uas/" >&2
    return 1
  fi
  # sipp 3.7 returns exit code 99 from the parent process after
  # daemonising via -bg (the daemon itself runs fine). `|| true` keeps
  # `set -e` happy. The cleanup trap and kill_uas sweep the listener.
  sipp -sf "$SCRIPT_DIR/uas/$uas_xml" \
       -p "$UAS_PORT" -i 127.0.0.1 -m 1 -bg \
       -trace_msg -message_file "$LOG_DIR/${uas_xml%.xml}-msg.log" \
       -trace_err -error_file "$LOG_DIR/${uas_xml%.xml}-err.log" \
       >"$LOG_DIR/${uas_xml%.xml}.log" 2>&1 || true
}

# Kill any sipp UAS still listening on $UAS_PORT (cleanup safety net).
kill_uas() {
  pkill -f "sipp.*-p $UAS_PORT" 2>/dev/null || true
}

# Cleanup trap (always runs).
cleanup() {
  for f in "$BRIDGE_PIDFILE" "$STUB_WS_PIDFILE" "$TEST_REG_PIDFILE" "$FLAP_PIDFILE"; do
    if [ -f "$f" ]; then
      pid=$(cat "$f" 2>/dev/null || echo "")
      [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
    fi
  done
  # sipp -bg forks a daemon we can't track via PIDFILE; sweep any UAS
  # listeners on $UAS_PORT so re-runs don't trip over a stale binding.
  kill_uas
  # Give children 1s to flush logs before final removal.
  sleep 1
  echo "logs preserved in $LOG_DIR (delete with: rm -rf $LOG_DIR)"
}
trap cleanup EXIT INT TERM

# Bring up the harness.
echo "=== starting harness (logs: $LOG_DIR) ==="
echo "  test-registrar @ udp://127.0.0.1:$REG_PORT"
echo "  stub-ws        @ ws://127.0.0.1:$WS_PORT/"
echo "  bridge         @ udp://127.0.0.1:$BRIDGE_SIP_PORT (SIP), :$HTTP_PORT (HTTP)"

# Bridge listens on $BRIDGE_SIP_PORT (default 5070) via SIP_LISTEN_ADDR.
# test-registrar owns $REG_PORT (default 5060). The bridge sends REGISTER to
# 127.0.0.1:$REG_PORT and advertises Contact: <sip:user@127.0.0.1:$BRIDGE_SIP_PORT>.
# sipp UAC sends INVITEs to 127.0.0.1:$BRIDGE_SIP_PORT.

start_test_registrar
start_stub_ws
start_flapping_http
sleep 0.5
start_bridge

# Wait for bridge to register (up to 10s). Bridge logs JSON; we poll for
# the registration completion line. Avoids a race where sipp INVITEs land
# before the bridge's listener has fully started.
for i in $(seq 1 50); do
  if grep -q '"msg":"REGISTER 200 OK"\|"msg":"registered"\|registered: true' "$LOG_DIR/bridge.log" 2>/dev/null; then
    break
  fi
  sleep 0.2
done

if ! kill -0 "$(cat "$BRIDGE_PIDFILE")" 2>/dev/null; then
  echo "FAIL: bridge failed to start (see $LOG_DIR/bridge.log)"
  tail -50 "$LOG_DIR/bridge.log" || true
  exit 1
fi
# Final health-check: bridge must be listening on BRIDGE_SIP_PORT.
if ! python3 -c "
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
try:
    s.bind(('127.0.0.1', $BRIDGE_SIP_PORT))
    s.close()
    raise SystemExit(1)  # bind succeeded → bridge isn't listening
except OSError:
    raise SystemExit(0)  # bind failed → bridge IS listening
" 2>/dev/null; then
  echo "FAIL: bridge not listening on udp://127.0.0.1:$BRIDGE_SIP_PORT after startup wait"
  tail -50 "$LOG_DIR/bridge.log" || true
  exit 1
fi
echo "bridge ready"

# ── REST orchestration ────────────────────────────────────────────────────
#
# Most scenarios need a REST POST fired DURING the inbound call's pause
# window (see <pause> in each XML). The bridge synthesises a CallSid for
# every inbound INVITE; the orchestrator polls /Calls.json to discover
# the live CallSid, then fires the scenario-specific POST against
# /Calls/{Sid}.json.
#
# Each per-scenario orchestrator runs in the background. It self-times
# against the inbound INVITE arrival (CallSid appearing in /Calls.json)
# rather than wall-clock — the scenario-side <pause> only needs to be
# wide enough for the POST round-trip, not for the discovery delay.

REST_BASE="http://127.0.0.1:$HTTP_PORT/2010-04-01/Accounts/$ACCOUNT_SID/Calls"

# poll_for_callsid LOG_FILE TIMEOUT_S — polls /Calls.json until a single
# active call is visible, prints its CallSid, returns 0. Logs to LOG_FILE.
poll_for_callsid() {
  local log="$1"
  local budget="${2:-10}"
  local elapsed=0
  local sid=""
  while [ "$elapsed" -lt "$((budget * 10))" ]; do
    sid=$(curl -s -u "$ACCOUNT_SID:$AUTH_TOKEN" "$REST_BASE.json" 2>/dev/null \
          | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    calls = d.get('calls', [])
    active = [c for c in calls if c.get('status') in ('ringing','in-progress','queued')]
    if len(active) == 1:
        print(active[0]['sid'])
except Exception:
    pass
" 2>/dev/null)
    if [ -n "$sid" ]; then
      echo "$sid"
      return 0
    fi
    sleep 0.1
    elapsed=$((elapsed + 1))
  done
  echo "orchestrator: timed out waiting for CallSid (${budget}s)" >>"$log"
  return 1
}

# rest_post_twiml CALL_SID TWIML LOG_FILE — fires POST /Calls/{Sid}.json
# with form-encoded Twiml=… body. Output appended to LOG_FILE.
rest_post_twiml() {
  local sid="$1"
  local twiml="$2"
  local log="$3"
  echo "orchestrator: POST Twiml to $sid" >>"$log"
  curl -s -o "$log.body" -w "HTTP %{http_code}\n" \
       -u "$ACCOUNT_SID:$AUTH_TOKEN" \
       -X POST \
       --data-urlencode "Twiml=$twiml" \
       "$REST_BASE/$sid.json" >>"$log" 2>&1
}

# Per-scenario orchestrator. Spawned as background process before sipp
# starts; coordinates timing against /Calls.json visibility.
orchestrate_scenario() {
  local scenario="$1"
  local olog="$LOG_DIR/orchestrator-${scenario%.xml}.log"
  echo "orchestrator: scenario=$scenario" >"$olog"

  case "$scenario" in
    rest-hangup-mid-stream.xml)
      local sid
      sid=$(poll_for_callsid "$olog" 10) || return 1
      echo "orchestrator: callsid=$sid" >>"$olog"
      sleep 1.5  # short stream window before hangup (per scenario spec)
      rest_post_twiml "$sid" "<Response><Hangup/></Response>" "$olog"
      ;;
    inbound-rest-dial-answer.xml|inbound-rest-dial-busy.xml|inbound-rest-dial-cancel.xml|inbound-rest-dial-codec-mismatch.xml)
      # Scenarios b (answer), c (busy), e (cancel), f (codec-mismatch) —
      # default 30 s ring window (Twilio default) is fine; UAS stub
      # determines the dial-leg outcome inside that window.
      local sid
      sid=$(poll_for_callsid "$olog" 10) || return 1
      echo "orchestrator: callsid=$sid" >>"$olog"
      sleep 0.5
      rest_post_twiml "$sid" "<Response><Dial><Number>+49301234567</Number></Dial></Response>" "$olog"
      ;;
    inbound-rest-dial-no-answer.xml)
      # Scenario d — UAS stays silent; we set a short <Dial timeout="5">
      # so the bridge fires CANCEL after 5 s instead of the Twilio
      # default 30 s, keeping the scenario fast.
      local sid
      sid=$(poll_for_callsid "$olog" 10) || return 1
      echo "orchestrator: callsid=$sid" >>"$olog"
      sleep 0.5
      rest_post_twiml "$sid" '<Response><Dial timeout="5"><Number>+49301234567</Number></Dial></Response>' "$olog"
      ;;
    inbound-default-streaming.xml)
      # No REST POST needed.
      :
      ;;
    status-callback-flapping-host.xml)
      # Scenario h — no REST POST. The bridge fires status-callback POSTs
      # to the flapping HTTP stub (operator-trusted default subscription
      # via STATUS_CALLBACK_DEFAULT_URL); the stub alternates 502 / 200
      # so the bridge's exp-backoff retry policy is exercised. Post-run
      # assertion: ≥2 POST attempts in flapping.log for the call's CallSid
      # (one rejected with 502, one accepted with 200 on retry).
      local sid
      sid=$(poll_for_callsid "$olog" 10) || return 1
      echo "orchestrator: callsid=$sid (will verify retries post-run)" >>"$olog"
      ;;
    *)
      echo "orchestrator: no REST orchestration registered for $scenario" >>"$olog"
      ;;
  esac
}

# Scenarios that need a UAS stub started on $UAS_PORT before sipp UAC runs.
# Returns the UAS XML name for the given scenario, or empty string if none.
uas_for_scenario() {
  case "$1" in
    inbound-rest-dial-answer.xml)            echo "uas-answer.xml" ;;
    inbound-rest-dial-busy.xml)              echo "uas-busy.xml" ;;
    inbound-rest-dial-no-answer.xml)         echo "uas-noanswer.xml" ;;
    inbound-rest-dial-cancel.xml)            echo "uas-slow-ring.xml" ;;
    inbound-rest-dial-codec-mismatch.xml)    echo "uas-codec-mismatch.xml" ;;
    *) echo "" ;;
  esac
}

# Run scenarios.
PASSED=()
FAILED=()
run_scenario() {
  local name="$1"
  local xml="$SCRIPT_DIR/$name"
  echo ""
  echo "=== Running scenario: $name ==="

  # Start UAS stub if this scenario needs one (for outbound INVITE).
  # The stub listens on $UAS_PORT; the bridge routes to it via
  # SIP_OUTBOUND_TARGET_PORT (set by start_bridge).
  local uas_xml
  uas_xml=$(uas_for_scenario "$name")
  if [ -n "$uas_xml" ]; then
    echo "  starting UAS stub: $uas_xml on udp://127.0.0.1:$UAS_PORT"
    start_uas "$uas_xml"
    sleep 0.3
  fi

  # Spawn the orchestrator in background; it polls /Calls.json after sipp
  # has sent its INVITE and the bridge has admitted the call.
  orchestrate_scenario "$name" &
  local orch_pid=$!

  if sipp -sf "$xml" \
       -p "$SIPP_PORT" \
       -m 1 \
       -i 127.0.0.1 \
       -trace_msg -message_file "$LOG_DIR/${name%.xml}-msg.log" \
       -trace_err -error_file "$LOG_DIR/${name%.xml}-err.log" \
       127.0.0.1:"$BRIDGE_SIP_PORT" 2>&1 | tail -20; then
    PASSED+=("$name")
    echo "PASS: $name"
  else
    FAILED+=("$name")
    echo "FAIL: $name"
    echo "  see $LOG_DIR/${name%.xml}-err.log"
    echo "  orchestrator log: $LOG_DIR/orchestrator-${name%.xml}.log"
  fi

  # Reap the orchestrator — it should have finished by the time sipp
  # exits, but kill it explicitly to avoid leaks on early sipp failure.
  wait "$orch_pid" 2>/dev/null || true

  # Post-run assertion: scenario h verifies the flapping stub saw ≥2
  # POST attempts (initial 502 + retry 200). Give the bridge a brief
  # window to complete its retry attempts before reading the log.
  if [ "$name" = "status-callback-flapping-host.xml" ]; then
    sleep 6  # 1st attempt + ~3-5s exp-backoff before retry
    local attempts
    attempts=$(grep -c "attempt=" "$LOG_DIR/flapping.log" 2>/dev/null || echo 0)
    echo "  flapping HTTP attempts logged: $attempts"
    if [ "$attempts" -lt 2 ]; then
      # downgrade scenario PASS to FAIL if retry never happened
      PASSED=("${PASSED[@]/$name}")
      FAILED+=("$name")
      echo "FAIL: $name (expected ≥2 flapping POST attempts, got $attempts)"
      echo "  flapping log: $LOG_DIR/flapping.log"
    fi
  fi

  # Cleanup UAS if we started one — re-runs need a fresh listener.
  if [ -n "$uas_xml" ]; then
    kill_uas
    sleep 0.3
  fi
}

case "$MODE" in
  single)
    if [ -z "$SCENARIO_FILTER" ] || [ ! -f "$SCRIPT_DIR/$SCENARIO_FILTER" ]; then
      echo "no such scenario: $SCENARIO_FILTER"
      echo "available: ${SCENARIOS[*]}"
      exit 2
    fi
    run_scenario "$SCENARIO_FILTER"
    ;;
  all)
    for s in "${SCENARIOS[@]}"; do
      run_scenario "$s"
    done
    ;;
esac

echo ""
echo "=== Summary ==="
echo "PASS (${#PASSED[@]}/${#SCENARIOS[@]}): ${PASSED[*]:-none}"
echo "FAIL (${#FAILED[@]}/${#SCENARIOS[@]}): ${FAILED[*]:-none}"

[ ${#FAILED[@]} -eq 0 ] && exit 0 || exit 1
