#!/usr/bin/env python3
"""Generate golden vectors for the X-Twilio-Signature signer using twilio-python.

This script is the upstream-of-record source for the Python half of the
cross-language byte-fidelity gate. It calls
``twilio.request_validator.RequestValidator.compute_signature`` against a
fixed list of fixtures and emits a JSON document to stdout suitable for
committing under
``go/internal/webhook/testdata/python_fixtures.json``.

REGENERATION
============

When bumping the pinned upstream library (currently ``twilio==9.5.*``) — or
when adding a new fixture name — re-run::

    python3 -m venv .venv && source .venv/bin/activate
    pip install 'twilio==9.5.*'
    python tools/gen_status_callback_fixtures/gen.py \\
        > go/internal/webhook/testdata/python_fixtures.json

The output path is given relative to the repository root.  After regen run::

    cd go && go test ./internal/webhook/... -run TestSign

A failure here means the upstream signer behavior drifted; that is a Twilio
release-note item that needs maintainer review before bumping the pin.

I-12 FIXTURE-SOURCE DISCLAIMER
==============================

Each entry carries ``source_lib`` and ``source_version`` fields recording
the upstream provenance.  Entries that are emitted with
``"source_version": "computed-locally-pending-upstream-verification"`` mean
the maintainer ran a local-compute fallback because pip / npm was not
available at commit time.  Those entries MUST be regenerated against pinned
upstream libs at the next opportunity.  The README.md tracks any such
entries as a TODO list; CI does NOT block on them but they flag follow-up
work.
"""
from __future__ import annotations

import json
import sys
from typing import Any, Dict, List

from twilio.request_validator import RequestValidator


# Each entry has the same shape as the JSON output (params is an OBJECT keyed
# by name → list-of-values, mirroring url.Values in Go).  The generator flattens
# multi-value lists for compute_signature() because twilio-python expects the
# tuple form for duplicate keys (see twilio/request_validator.py:get_values).
FIXTURES: List[Dict[str, Any]] = [
    # 1. Canonical Twilio fixture from twilio-python tests/unit/test_request_validator.py.
    {
        "name": "fixture_a_basic",
        "auth_token": "12345",
        "url": "https://mycompany.com/myapp.php?foo=1&bar=2",
        "params": {
            "CallSid": ["CA1234567890ABCDE"],
            "Digits": ["1234"],
            "From": ["+14158675309"],
            "To": ["+18005551212"],
            "Caller": ["+14158675309"],
        },
        "notes": "Canonical Twilio fixture from twilio-python "
                 "tests/unit/test_request_validator.py — proves the basic "
                 "single-value case end-to-end.",
    },
    # 2. Multi-value sort+dedupe — phase brief was wrong; this fixture proves
    #    SORT+DEDUPE is correct (NOT submission-order).
    {
        "name": "fixture_b_duplicate_values",
        "auth_token": "12345",
        "url": "https://mycompany.com/myapp.php?foo=1&bar=2",
        "params": {
            "Sid": ["CA123"],
            "SidAccount": ["AC123"],
            "Digits": ["5678", "1234", "1234"],
        },
        "notes": "Multi-value sort+dedupe per twilio-python "
                 "compute_signature(): for value in sorted(set(values)).",
    },
    # 3. Empty params — sign URL only (twilio-python's `if params:` short-circuit).
    {
        "name": "fixture_empty_params",
        "auth_token": "12345",
        "url": "https://customer.example/cb",
        "params": {},
        "notes": "Empty params — signature is HMAC-SHA1 of the URL bytes only.",
    },
    # 4. Single param.
    {
        "name": "fixture_single_param",
        "auth_token": "12345",
        "url": "https://customer.example/cb",
        "params": {
            "CallSid": ["CA1234567890abcdef1234567890abcdef"],
        },
        "notes": "Single-param boundary case.",
    },
    # 5. UTF-8 value — German umlauts. Verifies utf-8 encoding round-trip.
    {
        "name": "fixture_utf8_value",
        "auth_token": "12345",
        "url": "https://customer.example/cb",
        "params": {
            "Caller": ["Müller"],
            "Greeting": ["Grüß Gott"],
        },
        "notes": "UTF-8 byte-encode round-trip — German umlauts in values.",
    },
    # 6. Plus character in value (form-encoded space ambiguity).
    {
        "name": "fixture_special_chars_plus",
        "auth_token": "12345",
        "url": "https://customer.example/cb",
        "params": {
            "From": ["+4915123456789"],
            "Body": ["hello+world"],
        },
        "notes": "Plus characters in values must be signed verbatim "
                 "(NOT decoded to space).",
    },
    # 7. Percent character in value — must NOT be re-decoded.
    {
        "name": "fixture_special_chars_percent",
        "auth_token": "12345",
        "url": "https://customer.example/cb",
        "params": {
            "From": ["+1"],
            "Body": ["50%25 off"],
        },
        "notes": "Literal percent character in value — signer must not "
                 "URL-decode.",
    },
    # 8. Ampersand character in value.
    {
        "name": "fixture_special_chars_ampersand",
        "auth_token": "12345",
        "url": "https://customer.example/cb",
        "params": {
            "Body": ["rock & roll"],
            "Notes": ["a&b&c"],
        },
        "notes": "Ampersand in value must not terminate parsing on the "
                 "signing side.",
    },
    # 9. URL with explicit port — sign verbatim.
    {
        "name": "fixture_url_with_port",
        "auth_token": "12345",
        "url": "https://customer.example:8443/cb",
        "params": {
            "CallSid": ["CAabc"],
            "From": ["+1"],
        },
        "notes": "URL with explicit :8443 — sign verbatim (do NOT strip port).",
    },
    # 10. URL with query string — query bytes are part of the signed prefix.
    {
        "name": "fixture_url_with_query_string",
        "auth_token": "12345",
        "url": "https://customer.example/cb?account=AC1&v=1",
        "params": {
            "CallSid": ["CAabc"],
            "From": ["+1"],
        },
        "notes": "URL has ?account=AC1&v=1 — query bytes are part of the "
                 "signed URL prefix.",
    },
    # 11. Realistic terminal-event status callback payload.
    {
        "name": "fixture_status_callback_completed",
        "auth_token": "12345",
        "url": "https://customer.example/status",
        "params": {
            "CallSid": ["CAdeadbeef00000000000000000000abcd"],
            "AccountSid": ["ACdeadbeef00000000000000000000abcd"],
            "From": ["+4915123456789"],
            "To": ["+4930111222333"],
            "Direction": ["inbound"],
            "ApiVersion": ["2010-04-01"],
            "CallStatus": ["completed"],
            "Timestamp": ["Mon, 01 May 2026 18:00:00 +0000"],
            "SequenceNumber": ["3"],
            "CallbackSource": ["call-progress-events"],
            "CallDuration": ["42"],
            "Duration": ["1"],
            "SipResponseCode": ["200"],
            "Caller": ["+4915123456789"],
            "Called": ["+4930111222333"],
        },
        "notes": "Realistic terminal-event payload (CallStatus=completed) with "
                 "all status-callback form fields, including CallDuration, "
                 "Duration, SipResponseCode.",
    },
    # 12. Realistic non-terminal status callback payload (initiated/queued).
    {
        "name": "fixture_status_callback_initiated",
        "auth_token": "12345",
        "url": "https://customer.example/status",
        "params": {
            "CallSid": ["CAdeadbeef00000000000000000000abcd"],
            "AccountSid": ["ACdeadbeef00000000000000000000abcd"],
            "From": ["+4915123456789"],
            "To": ["+4930111222333"],
            "Direction": ["inbound"],
            "ApiVersion": ["2010-04-01"],
            "CallStatus": ["queued"],
            "Timestamp": ["Mon, 01 May 2026 18:00:00 +0000"],
            "SequenceNumber": ["0"],
            "CallbackSource": ["call-progress-events"],
            "Caller": ["+4915123456789"],
            "Called": ["+4930111222333"],
        },
        "notes": "Non-terminal (initiated/queued) payload — OMITS "
                 "CallDuration/Duration/SipResponseCode.",
    },
]


class _MultiDict:
    """Minimal MultiDict shim mirroring the Werkzeug interface that
    twilio-python's ``get_values()`` checks for first via ``getall``.

    twilio-python's ``compute_signature`` iterates ``sorted(set(params))``
    (which calls ``__iter__``) to enumerate unique key names, then calls
    ``get_values(params, name)`` per key to retrieve the per-key value list.
    For plain dicts this falls back to ``[param_dict[name]]`` — which
    returns a list-wrapped value, breaking multi-value keys.  Implementing
    ``getall`` makes twilio-python use the multi-value path verbatim.
    """

    def __init__(self, params: Dict[str, List[str]]) -> None:
        self._params = params

    def __iter__(self):
        # Return key names; sorted(set(self)) in compute_signature will dedupe.
        return iter(self._params.keys())

    def __bool__(self) -> bool:  # truthiness drives the `if params:` branch
        return bool(self._params)

    def getall(self, name: str) -> List[str]:
        return list(self._params.get(name, []))


def main() -> int:
    out: List[Dict[str, Any]] = []
    for f in sorted(FIXTURES, key=lambda x: x["name"]):
        validator = RequestValidator(f["auth_token"])
        sig = validator.compute_signature(f["url"], _MultiDict(f["params"]))
        entry = {
            "name": f["name"],
            "source_lib": "twilio-python",
            "source_version": "9.5.x",
            "auth_token": f["auth_token"],
            "url": f["url"],
            "params": f["params"],
            "expected_signature": sig,
            "notes": f.get("notes", ""),
        }
        out.append(entry)

    json.dump(out, sys.stdout, indent=2, ensure_ascii=False, sort_keys=False)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
