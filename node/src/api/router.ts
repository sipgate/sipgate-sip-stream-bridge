/**
 * Minimal, dependency-free route matcher for the Twilio-compatible REST control
 * plane. Replaces chi's path-pattern routing (go/internal/api/server.go Mount)
 * with a hand-rolled matcher over the exact three Call resource routes:
 *
 *   GET  /2010-04-01/Accounts/{AccountSid}/Calls.json            → list_calls
 *   GET  /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json  → get_call
 *   POST /2010-04-01/Accounts/{AccountSid}/Calls/{CallSid}.json  → modify_call
 *
 * matchRoute returns null for any path that is not one of these (the caller
 * falls through to /health, /metrics, or a 404). The {AccountSid} is extracted
 * verbatim — auth.basicAuth validates it constant-time against the configured
 * AccountSid, so the matcher itself does NOT shape-check it (a mismatch must
 * yield 401, not a routing miss → 404, which would leak account existence).
 */

import type { IncomingMessage } from 'node:http';

/** The bounded set of matched control-plane routes. */
export type ApiRouteName = 'list_calls' | 'get_call' | 'modify_call';

/** Result of a successful route match. callSid is present only for get/modify. */
export interface RouteMatch {
  route: ApiRouteName;
  accountSid: string;
  callSid?: string;
}

const API_PREFIX = '/2010-04-01/Accounts/';

/**
 * Strip the query string and decode the path component of a request URL.
 * node:http surfaces req.url as an origin-form string (path + optional query);
 * we only need the path for matching. URL parsing is done against a dummy base
 * so relative origin-form URLs parse cleanly.
 */
function pathOf(rawUrl: string | undefined): string {
  if (rawUrl === undefined || rawUrl === '') {
    return '';
  }
  const q = rawUrl.indexOf('?');
  const rawPath = q === -1 ? rawUrl : rawUrl.slice(0, q);
  try {
    return decodeURIComponent(rawPath);
  } catch {
    // Malformed percent-encoding — fall back to the raw path so a deliberate
    // bad-encoding probe still maps to a route (and then 404s on the segment
    // check) rather than throwing inside the router.
    return rawPath;
  }
}

/**
 * Match a request to one of the three Call routes. Returns null when the path
 * is not under /2010-04-01/Accounts/{AccountSid}/Calls*, the method does not
 * match the route, or the path shape is otherwise unrecognized.
 *
 * The method gate is part of the match: a POST to /Calls.json or a GET/PUT to a
 * path that only the matcher recognizes returns null (caller 404s) rather than
 * a 405 — this mirrors the Go chi behavior where unregistered method+path pairs
 * fall through.
 */
export function matchRoute(req: IncomingMessage): RouteMatch | null {
  const path = pathOf(req.url);
  if (!path.startsWith(API_PREFIX)) {
    return null;
  }
  const method = (req.method ?? 'GET').toUpperCase();
  // Remainder after the prefix: "{AccountSid}/Calls.json" or
  // "{AccountSid}/Calls/{CallSid}.json".
  const rest = path.slice(API_PREFIX.length);
  const slash = rest.indexOf('/');
  if (slash === -1) {
    return null;
  }
  const accountSid = rest.slice(0, slash);
  if (accountSid === '') {
    return null;
  }
  const tail = rest.slice(slash + 1); // "Calls.json" | "Calls/{CallSid}.json"

  if (tail === 'Calls.json') {
    if (method !== 'GET') {
      return null;
    }
    return { route: 'list_calls', accountSid };
  }

  if (tail.startsWith('Calls/') && tail.endsWith('.json')) {
    const callSid = tail.slice('Calls/'.length, tail.length - '.json'.length);
    // A further slash would mean a sub-resource (Notifications.json etc.) which
    // we do not route here.
    if (callSid === '' || callSid.includes('/')) {
      return null;
    }
    if (method === 'GET') {
      return { route: 'get_call', accountSid, callSid };
    }
    if (method === 'POST') {
      return { route: 'modify_call', accountSid, callSid };
    }
    return null;
  }

  return null;
}
