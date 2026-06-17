// The static UI is served unauthenticated. The operator logs in via the app's
// own form; the token (AUTH_TOKEN) is held in memory/sessionStorage and sent
// EXPLICITLY as Basic AccountSid:token on each API call. The browser never
// caches credentials, and the Twilio REST API only ever sees AccountSid:token.

const JSON_HEADERS = { Accept: 'application/json' };

/** Authentication error carries .status so callers can distinguish 401. */
export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

function authHeader(accountSid, token) {
  return 'Basic ' + btoa(`${accountSid}:${token}`);
}

/** GET /health (unauthenticated) → { registered, account_sid, active_calls, active_forwards }. */
export async function getHealth() {
  const res = await fetch('/health', { headers: JSON_HEADERS });
  if (!res.ok) throw new ApiError(`/health returned ${res.status}`, res.status);
  return res.json();
}

/**
 * GET the (paginated) Calls list, authenticated with AccountSid:token.
 * Throws ApiError(status=401) on bad credentials so the caller can log out.
 */
export async function listCalls(accountSid, token, pageSize = 200) {
  const url = `/2010-04-01/Accounts/${encodeURIComponent(accountSid)}/Calls.json?PageSize=${pageSize}`;
  const res = await fetch(url, {
    headers: { ...JSON_HEADERS, Authorization: authHeader(accountSid, token) },
  });
  if (!res.ok) throw new ApiError(`Calls.json returned ${res.status}`, res.status);
  return res.json();
}
