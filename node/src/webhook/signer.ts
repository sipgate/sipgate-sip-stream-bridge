/**
 * X-Twilio-Signature computation for outbound webhook POSTs.
 *
 * Byte-identical to twilio-python RequestValidator.compute_signature and to
 * twilio-node getExpectedTwilioSignature. Ported from the Go signer
 * (internal/webhook/signer.go) — see the golden-vector fixtures in
 * test/webhook.signer.test.ts for the cross-language byte-fidelity gate.
 *
 * Pure function — no side effects, no I/O.
 */

import { createHmac } from 'node:crypto';

/**
 * Multi-value form params: each key maps to its list of submitted values.
 * Single-value keys still use a one-element array, matching url.Values / the
 * fixture JSON shape.
 */
export type SignParams = Record<string, string[]>;

/**
 * Normalise the accepted param representations into a plain multi-map.
 * URLSearchParams is flattened preserving per-key insertion order; later
 * dedupe+sort makes order irrelevant to the output but we keep it faithful.
 */
function toMultiMap(params: SignParams | URLSearchParams): SignParams {
  if (params instanceof URLSearchParams) {
    const out: SignParams = {};
    for (const [k, v] of params) {
      (out[k] ??= []).push(v);
    }
    return out;
  }
  return params;
}

/**
 * Compute the X-Twilio-Signature for an outbound webhook.
 *
 * Six load-bearing details (verified against both upstream libs and the
 * golden-vector fixtures):
 *
 *  1. Param names sorted by case-sensitive ASCII byte order ('A'<'Z'<'a'<'z').
 *  2. No delimiter between key and value, nor between pairs (s += k + v).
 *  3. base64 standard alphabet, with '=' padding.
 *  4. URL is signed verbatim — caller is responsible for URL fidelity.
 *  5. Multi-value keys: DEDUPE then SORT values per key (case-sensitive
 *     ASCII), NOT submission order — see fixture_b_duplicate_values.
 *  6. Values concatenated RAW, NOT URL-encoded ('+' stays '+', UTF-8 bytes
 *     stay verbatim).
 *
 * If params is empty, the signature is HMAC-SHA1 over the URL bytes only.
 *
 * @param authToken customer's Twilio AuthToken (raw string, NOT base64-decoded)
 * @param url       literal URL bytes that appear on the request line
 * @param params    form params (multi-map or URLSearchParams); empty for GET
 */
export function sign(
  authToken: string,
  url: string,
  params: SignParams | URLSearchParams,
): string {
  const map = toMultiMap(params);

  // Detail 1: case-sensitive ASCII byte order. localeCompare is locale- and
  // case-insensitive in some locales, so sort by raw code units (the default
  // Array.sort comparator), which is the ASCII byte order for the range
  // Twilio uses and matches Go's sort.Strings / Python str sort / JS sort.
  const keys = Object.keys(map).sort();

  let s = url;
  for (const k of keys) {
    const values = map[k];
    // Detail 5: dedupe (exact match) then sort the deduped values.
    const unique = Array.from(new Set(values)).sort();
    for (const v of unique) {
      // Detail 2 + 6: no delimiter, raw value bytes.
      s += k + v;
    }
  }

  // Detail 3: HMAC-SHA1, standard base64 with '=' padding. The message is
  // encoded as UTF-8 bytes (Node's default for a string passed to update()).
  return createHmac('sha1', authToken).update(s, 'utf8').digest('base64');
}
