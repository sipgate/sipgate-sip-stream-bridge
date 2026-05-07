# Status-Callback Signature Fixture Generators

Build-time codegen for the X-Twilio-Signature golden vectors used by
the cross-language byte-fidelity gate.

## Why these scripts exist

The headline technical claim of the audio-dock signer is that
`webhook.Sign(authToken, urlStr, params)` is **byte-identical** to the
upstream Twilio reference signers
(`twilio-python.RequestValidator.compute_signature` and
`twilio-node.getExpectedTwilioSignature`).  One byte of drift between our
output and the customer's `RequestValidator.validate(...)` means a 100 %
rejection rate at the customer's bot — a regression that would break every
production integration silently.

The two generators below run the upstream signers against a fixed list of
12 inputs and emit the resulting JSON documents.  Those documents are
committed to the Go test tree so that CI can replay them offline (no
network at test time) and fail hard on any divergence.

## Pinned upstream versions

| Generator | Library | Pin            | Resolves to (verified)         |
|-----------|---------|----------------|---------------------------------|
| `gen.py`  | twilio-python | `twilio==9.5.*` | `twilio` 9.5.2 (May 2026)       |
| `gen.js`  | twilio-node   | `twilio@5`      | `twilio` 5.x (May 2026)         |

The pinned majors are the latest stable releases at the time the gate was
introduced.  Upstream commits to `main` were re-verified against
`request_validator.py` and `webhooks.ts` on 2026-05-01.

## When to regenerate

**Only when bumping an upstream library major or adding a new fixture
name.**  The committed JSON files are the "source of truth" for CI; the
scripts are an audit/regen path, not a step in the normal build.

If you bump a library and the generated signatures change, that is a
Twilio release-note item that needs maintainer review.  The cross-language
parity test (`TestSign_CrossLibraryParity`) will fail on any divergence
between Python and Node — also worth a closer look before committing the
new fixtures.

## Regen procedure

### Python

```bash
# From the repository root.
python3 -m venv /tmp/twilio-py-gen
source /tmp/twilio-py-gen/bin/activate
pip install --quiet 'twilio==9.5.*'

python tools/gen_status_callback_fixtures/gen.py \
    > go/internal/webhook/testdata/python_fixtures.json
```

### Node

```bash
# From the repository root.
mkdir -p /tmp/twilio-node-gen && cd /tmp/twilio-node-gen
npm init -y >/dev/null
npm install --silent --no-audit --no-fund 'twilio@5'

cd "$(git rev-parse --show-toplevel)"
TWILIO_NODE_PATH=/tmp/twilio-node-gen/node_modules \
    node tools/gen_status_callback_fixtures/gen.js \
    > go/internal/webhook/testdata/node_fixtures.json
```

(The `TWILIO_NODE_PATH` env var is the helper documented in `gen.js` for
sourcing twilio-node from a temp install dir without polluting the
repository's own dependency tree.)

### Verification step

After both generators have written their output, run:

```bash
cd go && go test ./internal/webhook/... -run TestSign
```

A failure here means one of three things:

1. The Go signer regressed (most likely — fix `signer.go`).
2. Upstream twilio-python or twilio-node changed semantics with the bump
   (read the release notes; the gate is doing its job).
3. The cross-language parity broke between Python and Node (the rarest,
   but historically has happened — needs maintainer judgement on which
   side to chase).

## Output paths

Both generators write to `go/internal/webhook/testdata/`:

- `python_fixtures.json` — twilio-python output
- `node_fixtures.json` — twilio-node output

Both are committed to the repository.  They are read by `signer_test.go`
via `loadFixtures(t, name)`.

## Fixture coverage

The 12 fixtures shared between the two generators cover the cases
documented in RESEARCH §2.2:

| #  | Name                                  | What it covers |
|----|---------------------------------------|----------------|
| 1  | `fixture_a_basic`                     | Canonical Twilio fixture from `twilio-python` test suite (5 single-value params) — produces `RSOYDt4T1cUTdK1PDd93/VVr8B8=`. |
| 2  | `fixture_b_duplicate_values`          | Multi-value sort+dedupe — produces `IK+Dwps556ElfBT0I3Rgjkr1wJU=`. Proves Detail 5 (NOT submission-order). |
| 3  | `fixture_empty_params`                | URL-only signing (no params) — exercises the upstream `if params:` short-circuit. |
| 4  | `fixture_single_param`                | Minimum boundary. |
| 5  | `fixture_utf8_value`                  | German umlauts (`Müller`, `Grüß Gott`) — UTF-8 byte-encode round-trip. |
| 6  | `fixture_special_chars_plus`          | `+` in value — must be signed verbatim, not decoded to space. |
| 7  | `fixture_special_chars_percent`       | Literal `%` in value — must NOT be re-decoded. |
| 8  | `fixture_special_chars_ampersand`     | `&` in value — must NOT terminate parsing. |
| 9  | `fixture_url_with_port`               | URL with explicit `:8443` — sign verbatim (do NOT strip). |
| 10 | `fixture_url_with_query_string`       | URL with `?account=…&v=…` — query bytes are part of the signed prefix. |
| 11 | `fixture_status_callback_completed`   | Realistic terminal-event payload (CallStatus=completed) with the full §3.3 form-field set. |
| 12 | `fixture_status_callback_initiated`   | Realistic non-terminal payload (CallStatus=queued) — omits `CallDuration` / `Duration` / `SipResponseCode`. |

## I-12 fixture-source disclaimer

Each fixture entry's `source_lib` and `source_version` fields carry the
upstream provenance.  Entries that ship with

```
"source_version": "computed-locally-pending-upstream-verification"
```

mean a fallback path was used (no pip / no npm available at commit time).
Those entries MUST be regenerated against pinned upstream libs at the next
opportunity.  CI does not block on them, but the running TODO list below
flags follow-up work for the next maintainer pass.

### Locally-computed entries TODO

_(none — all 12 entries verified against twilio-python 9.5.2 and
twilio-node 5.x as of 2026-05-04)_

When a regen falls back to local-compute (e.g. CI runner without
network), add the affected fixture names here in the form

```markdown
- `fixture_xxx` — needs upstream verification against `twilio==9.5.*`
- `fixture_yyy` — needs upstream verification against `twilio@5`
```

and the next maintainer with both packages installed re-runs the
generators and removes the entries.
