# Incident Response — Credential Rotation

Operator runbook for credential rotation. Part of the v3.0 hardening +
release-gate documentation set. See
[RELEASE_CHECKLIST.md](./RELEASE_CHECKLIST.md) for the pre-merge
checklist; the AUTH_TOKEN-rotation item references this runbook.

The bridge handles two long-lived credentials:

- `AUTH_TOKEN` — Twilio-compatible Basic Auth secret AND the HMAC-SHA1
  signer key for outbound `X-Twilio-Signature` headers. Affects REST
  API authentication AND signature verification by customer endpoints.
- `SIP_PASSWORD` — sipgate trunk registration password. Affects SIP
  REGISTER + outbound INVITE digest authentication (401/407 response
  handling).

## Symptom

- Suspected `AUTH_TOKEN` leak (the secret appeared in a logged URL,
  paste-bin, customer support transcript, public git commit, etc.).
- Suspected `SIP_PASSWORD` leak.
- Unexpected REST API authentication patterns (auth from unfamiliar IPs,
  unusual call-modify volume).
- Unexpected SIP registration churn (`sip_registration_status` flapping
  in Grafana).
- Compliance team mandates rotation per scheduled cadence.

## Important caveat — secret masking is NOT a substitute for rotation

The bridge masks `AUTH_TOKEN` and `SIP_PASSWORD` in its log output via
`internal/observability/logging.go` (`NewSecretMaskWriter`) — they
appear as `***` in any zerolog emission. **However:**

- Secrets in **environment variables** are NOT masked — `kubectl
  describe pod` and `/proc/<pid>/environ` (any process with the same UID)
  show them in the clear.
- Secrets in **Kubernetes Secret manifests** are base64-encoded but NOT
  encrypted at rest unless your cluster has KMS encryption enabled.
- Secrets pinned to **CI environment** are typically logged in build
  metadata and visible to anyone with read access to the CI history.

If a secret has plausibly traversed any of the above surfaces unmasked,
**rotate** — do not assume the log mask saved you.

## AUTH_TOKEN rotation

`AUTH_TOKEN` is the secret that:

1. Authenticates inbound REST API requests via HTTP Basic Auth (constant-
   time compared in `go/internal/api/auth.go`).
2. Signs outbound HTTP requests (status callbacks, action callbacks) via
   HMAC-SHA1 in the `X-Twilio-Signature` header.
3. Authorises customer-side signature verification (their middleware
   uses the same secret to verify our signature).

### Procedure

1. **Generate a new token.** Use a CSPRNG; 32 bytes URL-safe base64 is
   the Twilio-compatible shape:
   ```bash
   openssl rand -base64 32 | tr -d '=' | tr '+/' '-_'
   ```

2. **Stage the new token alongside the old.** During rotation, customer
   middleware MUST accept signatures from EITHER token to avoid a
   verification gap. The bridge does NOT support dual-secret signing in
   v3.0 — the rotation is single-cutover. Coordinate the cutover window
   with the customer:
   - T-0: customer adds the new token to their verifier as a secondary
     accepted secret.
   - T+1: bridge swap (steps 3-5 below).
   - T+2: customer removes the old token from their verifier.

3. **Update the deployment env.** k8s example:
   ```bash
   # Update the secret
   kubectl create secret generic sipgate-bridge-auth \
     --from-literal=AUTH_TOKEN="<new-token>" \
     --dry-run=client -o yaml \
     | kubectl apply -f -

   # Roll the bridge to pick up the new env
   kubectl rollout restart deployment/sipgate-bridge
   kubectl rollout status deployment/sipgate-bridge --timeout=60s
   ```

4. **Verify the cutover.**
   - REST API: send a synthetic GET with the new token; expect 200.
   - REST API: send a synthetic GET with the OLD token; expect 401.
   - Status callback: trigger a synthetic call that emits a callback;
     verify the customer's verifier accepts the new signature.

5. **Audit the previous-token blast radius.** Pull REST access logs
   for the period the old token was valid:
   ```bash
   kubectl logs -l app=sipgate-bridge --since=24h \
     | jq 'select(.path | startswith("/2010-04-01/Accounts/"))
           | {ts: .time, ip: .remote_addr, route: .route, status: .status}'
   ```
   Look for unfamiliar source IPs, unusual call-modify volume, or 401s
   that suggest scanning attempts.

### What does NOT need a rotation

- An `AUTH_TOKEN` value that NEVER traversed an unmasked surface
  (i.e. it was provisioned via Vault → env-injection → Kubernetes Secret
  with KMS-encrypted etcd; never echoed to a log; never committed to
  git; never sent to chat). For these, scheduled rotation per
  compliance cadence is sufficient.
- The bridge log lines themselves — secret masking IS in effect for
  zerolog emissions; the log file alone is not a leak surface.

## SIP_PASSWORD rotation

`SIP_PASSWORD` authenticates the bridge to the sipgate registrar. A
rotation requires sipgate-side coordination because the trunk's
credential database is the source of truth.

### Procedure

1. **Coordinate with sipgate trunk operations** to provision a new
   password against the same SIP user.

2. **Stage the new password.** Update the deployment env:
   ```bash
   kubectl create secret generic sipgate-bridge-sip \
     --from-literal=SIP_PASSWORD="<new-password>" \
     --dry-run=client -o yaml | kubectl apply -f -
   ```

3. **Trigger graceful rollout.** The drain budget (15s, locked in v3.0)
   plus K8s 30s `terminationGracePeriodSeconds` covers a clean cutover:
   ```bash
   kubectl rollout restart deployment/sipgate-bridge
   ```
   Existing in-flight calls drain via `DrainAll` → SIP UNREGISTER → BYE
   per the v3.0 hardening; the new pod registers with the new password
   on startup.

4. **Verify the new registration.**
   - `sip_registration_status` returns to 1 within 30s of pod-ready.
   - `/health` returns the locked 4-field JSON with `registered=true`.
   - One synthetic inbound call completes the WS handshake (proves the
     new registration is accepted by the trunk for inbound INVITEs).

5. **Notify sipgate** — the old password should be removed on the trunk
   side once the bridge reports stable registration with the new
   password. Until then, EITHER password remains usable; this is
   safer than a hard cutover (the trunk's grace window prevents a
   registration gap if the bridge can't reach the registrar promptly
   after restart).

## sipgate trunk re-registration

A successful credential rotation produces a SIP REGISTER with the new
credentials. If the trunk rejects the registration after rotation:

| Symptom                                         | Likely cause                                          | Mitigation                                                 |
| ----------------------------------------------- | ----------------------------------------------------- | ---------------------------------------------------------- |
| `401 Unauthorized` on REGISTER                  | new password not propagated trunk-side                | sipgate ops re-applies the secret; re-test                 |
| `403 Forbidden` on REGISTER                     | account suspended or DID set changed                  | escalate to sipgate trunk operations                       |
| Registration loops (`sip_registration_status` flapping) | clock skew (Digest nonce drift) OR network MTU issue  | check NTP sync; check IP MTU between bridge and trunk      |

## Suspected leak workflow (high-priority)

If a leak is *active* (e.g. attacker observed using the old token in
real time):

1. **Cut access immediately** — rotate without the customer-coordinated
   T-0 / T+1 / T+2 staging if you assess the leak risk exceeds the
   single-cutover verification gap.
2. **Block source IPs** at the K8s ingress / load balancer if a finite
   set of attacker IPs is identifiable.
3. **Audit the blast radius** — identify which calls were modified via
   the compromised token; correlate with status-callback logs to find
   any unauthorised `<Dial>` that toll-fraud guardrails MAY have failed
   to catch.
4. **Notify customer + compliance** — per sipgate's incident-response
   playbook (placeholder; operator team to fill at deploy time).

## Related runbooks

- [TOLL_FRAUD.md](./TOLL_FRAUD.md) — toll-fraud guardrail config
  (compromised tokens often manifest as toll-fraud rejections first).
- [CALLER_ID.md](./CALLER_ID.md) — caller-ID policy (a compromised
  token may be used to spoof caller-ID).
- [DASHBOARDS.md](./DASHBOARDS.md) — Grafana panels for unusual REST
  API patterns and registration flapping.
- [SESSION_TIMERS.md](./SESSION_TIMERS.md) — session-timer rejection is
  benign; rotation does not affect it.

## Reference

- `go/internal/observability/logging.go` — `NewSecretMaskWriter`
  contract (mask is on `io.Writer`; honours all zerolog emissions but
  NOT the env)
- `go/internal/api/auth.go` — `subtle.ConstantTimeCompare` (timing-safe
  Basic Auth comparison)
- OWASP Authentication Cheat Sheet
- Twilio Auth Token rotation guidance:
  https://help.twilio.com/articles/223136027 (external; Twilio's own
  rotation playbook is the conceptual model for our compatibility-first
  REST surface)
