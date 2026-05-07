#!/usr/bin/env node
/**
 * Generate golden vectors for the X-Twilio-Signature signer using twilio-node.
 *
 * This script is the upstream-of-record source for the Node half of the
 * cross-language byte-fidelity gate. It calls
 * `twilio.webhooks.getExpectedTwilioSignature` against a fixed list of
 * fixtures and emits a JSON document to stdout suitable for committing
 * under `go/internal/webhook/testdata/node_fixtures.json`.
 *
 * REGENERATION
 * ============
 *
 * When bumping the pinned upstream library (currently `twilio@5.x`) — or
 * when adding a new fixture name — re-run from the repository root:
 *
 *     mkdir -p /tmp/twilio-node-gen && cd /tmp/twilio-node-gen
 *     npm init -y >/dev/null && npm install twilio@5
 *     cd /Users/rotmanov/git/sipgate/audio-dock
 *     node tools/gen_status_callback_fixtures/gen.js \
 *         > go/internal/webhook/testdata/node_fixtures.json
 *
 * After regen run:
 *
 *     cd go && go test ./internal/webhook/... -run TestSign
 *
 * A failure here means the upstream signer behavior drifted; that is a
 * Twilio release-note item that needs maintainer review before bumping
 * the pin.
 *
 * I-12 FIXTURE-SOURCE DISCLAIMER
 * ==============================
 *
 * Each entry carries `source_lib` and `source_version` fields recording
 * the upstream provenance.  Entries that ship with `source_version` set to
 * `"computed-locally-pending-upstream-verification"` mean the maintainer
 * ran a local-compute fallback because npm was not available at commit
 * time.  Those entries MUST be regenerated against pinned upstream libs at
 * the next opportunity.  The README.md tracks any such entries as a TODO
 * list; CI does NOT block on them but they flag follow-up work.
 */
"use strict";

// twilio-node v5 publishes the helper under twilio/lib/webhooks/webhooks.
// `getExpectedTwilioSignature(authToken, url, params)` matches the upstream
// `compute_signature` byte-for-byte.
//
// We resolve the package via the conventional require path; the regen
// command above ensures `twilio` is reachable in NODE_PATH.
let getExpectedTwilioSignature;
try {
  // eslint-disable-next-line global-require
  ({ getExpectedTwilioSignature } = require("twilio/lib/webhooks/webhooks"));
} catch (errPrimary) {
  // Fallback: when running from a temporary install dir (the README's
  // recommended regen flow uses /tmp/twilio-node-gen/node_modules), let the
  // caller point us at a specific node_modules tree via TWILIO_NODE_PATH.
  const path = require("path");
  const cand = process.env.TWILIO_NODE_PATH
    ? path.resolve(process.env.TWILIO_NODE_PATH, "twilio/lib/webhooks/webhooks")
    : null;
  if (cand) {
    // eslint-disable-next-line global-require
    ({ getExpectedTwilioSignature } = require(cand));
  } else {
    process.stderr.write(
      "gen.js: cannot resolve twilio-node. Either install it locally\n" +
        "        (npm install twilio@5 in this directory) or set\n" +
        "        TWILIO_NODE_PATH=/path/to/node_modules.\n" +
        "        Underlying error: " +
        errPrimary.message +
        "\n",
    );
    process.exit(1);
  }
}

// FIXTURES MUST mirror tools/gen_status_callback_fixtures/gen.py exactly:
// same names, same auth_token, same url, same params (including value lists).
// Cross-language parity is asserted in the Go test.
const FIXTURES = [
  {
    name: "fixture_a_basic",
    auth_token: "12345",
    url: "https://mycompany.com/myapp.php?foo=1&bar=2",
    params: {
      CallSid: ["CA1234567890ABCDE"],
      Digits: ["1234"],
      From: ["+14158675309"],
      To: ["+18005551212"],
      Caller: ["+14158675309"],
    },
    notes:
      "Canonical Twilio fixture from twilio-python " +
      "tests/unit/test_request_validator.py — proves the basic " +
      "single-value case end-to-end.",
  },
  {
    name: "fixture_b_duplicate_values",
    auth_token: "12345",
    url: "https://mycompany.com/myapp.php?foo=1&bar=2",
    params: {
      Sid: ["CA123"],
      SidAccount: ["AC123"],
      Digits: ["5678", "1234", "1234"],
    },
    notes:
      "Multi-value sort+dedupe per twilio-node " +
      "toFormUrlEncodedParam: Array.from(new Set(paramValue)).sort().",
  },
  {
    name: "fixture_empty_params",
    auth_token: "12345",
    url: "https://customer.example/cb",
    params: {},
    notes: "Empty params — signature is HMAC-SHA1 of the URL bytes only.",
  },
  {
    name: "fixture_single_param",
    auth_token: "12345",
    url: "https://customer.example/cb",
    params: {
      CallSid: ["CA1234567890abcdef1234567890abcdef"],
    },
    notes: "Single-param boundary case.",
  },
  {
    name: "fixture_utf8_value",
    auth_token: "12345",
    url: "https://customer.example/cb",
    params: {
      Caller: ["Müller"],
      Greeting: ["Grüß Gott"],
    },
    notes: "UTF-8 byte-encode round-trip — German umlauts in values.",
  },
  {
    name: "fixture_special_chars_plus",
    auth_token: "12345",
    url: "https://customer.example/cb",
    params: {
      From: ["+4915123456789"],
      Body: ["hello+world"],
    },
    notes:
      "Plus characters in values must be signed verbatim " +
      "(NOT decoded to space).",
  },
  {
    name: "fixture_special_chars_percent",
    auth_token: "12345",
    url: "https://customer.example/cb",
    params: {
      From: ["+1"],
      Body: ["50%25 off"],
    },
    notes:
      "Literal percent character in value — signer must not " +
      "URL-decode.",
  },
  {
    name: "fixture_special_chars_ampersand",
    auth_token: "12345",
    url: "https://customer.example/cb",
    params: {
      Body: ["rock & roll"],
      Notes: ["a&b&c"],
    },
    notes:
      "Ampersand in value must not terminate parsing on the " +
      "signing side.",
  },
  {
    name: "fixture_url_with_port",
    auth_token: "12345",
    url: "https://customer.example:8443/cb",
    params: {
      CallSid: ["CAabc"],
      From: ["+1"],
    },
    notes:
      "URL with explicit :8443 — sign verbatim (do NOT strip port).",
  },
  {
    name: "fixture_url_with_query_string",
    auth_token: "12345",
    url: "https://customer.example/cb?account=AC1&v=1",
    params: {
      CallSid: ["CAabc"],
      From: ["+1"],
    },
    notes:
      "URL has ?account=AC1&v=1 — query bytes are part of the " +
      "signed URL prefix.",
  },
  {
    name: "fixture_status_callback_completed",
    auth_token: "12345",
    url: "https://customer.example/status",
    params: {
      CallSid: ["CAdeadbeef00000000000000000000abcd"],
      AccountSid: ["ACdeadbeef00000000000000000000abcd"],
      From: ["+4915123456789"],
      To: ["+4930111222333"],
      Direction: ["inbound"],
      ApiVersion: ["2010-04-01"],
      CallStatus: ["completed"],
      Timestamp: ["Mon, 01 May 2026 18:00:00 +0000"],
      SequenceNumber: ["3"],
      CallbackSource: ["call-progress-events"],
      CallDuration: ["42"],
      Duration: ["1"],
      SipResponseCode: ["200"],
      Caller: ["+4915123456789"],
      Called: ["+4930111222333"],
    },
    notes:
      "Realistic terminal-event payload (CallStatus=completed) with " +
      "all status-callback form fields, including CallDuration, " +
      "Duration, SipResponseCode.",
  },
  {
    name: "fixture_status_callback_initiated",
    auth_token: "12345",
    url: "https://customer.example/status",
    params: {
      CallSid: ["CAdeadbeef00000000000000000000abcd"],
      AccountSid: ["ACdeadbeef00000000000000000000abcd"],
      From: ["+4915123456789"],
      To: ["+4930111222333"],
      Direction: ["inbound"],
      ApiVersion: ["2010-04-01"],
      CallStatus: ["queued"],
      Timestamp: ["Mon, 01 May 2026 18:00:00 +0000"],
      SequenceNumber: ["0"],
      CallbackSource: ["call-progress-events"],
      Caller: ["+4915123456789"],
      Called: ["+4930111222333"],
    },
    notes:
      "Non-terminal (initiated/queued) payload — OMITS " +
      "CallDuration/Duration/SipResponseCode.",
  },
];

// twilio-node's `getExpectedTwilioSignature` accepts params as a plain
// object: keys are sorted, each value is either a scalar or an array (the
// array path is exercised in `toFormUrlEncodedParam` — it does
// `Array.from(new Set(paramValue)).sort()`).
//
// To preserve cross-language parity with the Python generator we always
// pass the SAME shape:
//   - single-value keys → bare string (mirrors the Python single-value
//     branch where _MultiDict.getall returns a length-1 list and Twilio
//     does sorted(set([v])) which is just [v]).
//   - multi-value keys → array (exercises toFormUrlEncodedParam's array
//     branch, which dedupes+sorts — identical to Python's `sorted(set())`).
function paramsForNode(params) {
  const out = {};
  for (const [k, vs] of Object.entries(params)) {
    if (vs.length === 1) {
      out[k] = vs[0];
    } else {
      out[k] = vs.slice();
    }
  }
  return out;
}

function main() {
  const sorted = FIXTURES.slice().sort((a, b) =>
    a.name < b.name ? -1 : a.name > b.name ? 1 : 0,
  );
  const out = sorted.map((f) => {
    const sig = getExpectedTwilioSignature(
      f.auth_token,
      f.url,
      paramsForNode(f.params),
    );
    return {
      name: f.name,
      source_lib: "twilio-node",
      source_version: "5.x",
      auth_token: f.auth_token,
      url: f.url,
      params: f.params,
      expected_signature: sig,
      notes: f.notes || "",
    };
  });
  process.stdout.write(JSON.stringify(out, null, 2) + "\n");
}

main();
