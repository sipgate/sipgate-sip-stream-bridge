/**
 * Twilio-compatible REST error bodies for the control-plane HTTP surface.
 *
 * ApiError is byte-identical in JSON shape to the Twilio REST API error body so
 * SDKs (twilio-node, twilio-python, …) validate it against their typed error
 * wrappers:
 *
 *   {"code":N,"message":"...","more_info":"https://www.twilio.com/docs/errors/N","status":HHH}
 *
 * Pure leaf module: framework-agnostic, depends only on node:http (the project
 * serves via raw http.createServer, not a framework).
 */

import type { ServerResponse } from 'node:http';

/** HTTP status constants used by the prebuilt constructors (named for clarity). */
const STATUS_BAD_REQUEST = 400;
const STATUS_UNAUTHORIZED = 401;
const STATUS_NOT_FOUND = 404;
const STATUS_REQUEST_ENTITY_TOO_LARGE = 413;
const STATUS_TOO_MANY_REQUESTS = 429;

/**
 * ApiError is the Twilio-shaped REST error body. The JSON field names are
 * snake_case (more_info) to match Twilio's wire contract exactly. `moreInfo`
 * is always Twilio's canonical pattern https://www.twilio.com/docs/errors/<code>.
 */
export class ApiError {
  readonly code: number;
  readonly message: string;
  readonly more_info: string;
  readonly status: number;

  constructor(code: number, message: string, status: number) {
    this.code = code;
    this.message = message;
    this.more_info = `https://www.twilio.com/docs/errors/${code}`;
    this.status = status;
  }

  /**
   * Serialize to the exact Twilio-shape JSON object (snake_case keys, fixed
   * field order). Returned as a plain object so JSON.stringify emits only the
   * wire fields and never class internals.
   */
  toJSON(): { code: number; message: string; more_info: string; status: number } {
    return {
      code: this.code,
      message: this.message,
      more_info: this.more_info,
      status: this.status,
    };
  }

  /**
   * Set Content-Type, write the HTTP status, and emit the JSON body to a raw
   * node:http ServerResponse.
   *
   * Order is significant: setHeader must precede writeHead, and writeHead must
   * precede the body write. Call once per response.
   */
  writeJSON(res: ServerResponse): void {
    res.setHeader('Content-Type', 'application/json');
    res.writeHead(this.status);
    res.end(JSON.stringify(this.toJSON()));
  }
}

/**
 * ErrAuthRequired returns the Twilio 20003 / 401 "Authentication Error" error.
 * Used by Basic Auth on missing or invalid credentials.
 */
export function ErrAuthRequired(): ApiError {
  return new ApiError(20003, 'Authentication Error - No credentials provided', STATUS_UNAUTHORIZED);
}

/**
 * ErrNotFound returns the Twilio 20404 / 404 "resource not found" error.
 * `resource` is interpolated into the message for parity with Twilio's wording.
 */
export function ErrNotFound(resource: string): ApiError {
  return new ApiError(20404, `The requested resource ${resource} was not found`, STATUS_NOT_FOUND);
}

/**
 * ErrInvalidParams returns the Twilio 21218 / 400 "Invalid parameters" error.
 * `detail` is appended after a colon for caller-supplied context.
 */
export function ErrInvalidParams(detail: string): ApiError {
  return new ApiError(21218, `Invalid parameters: ${detail}`, STATUS_BAD_REQUEST);
}

/**
 * ErrCallNotInProgress returns the Twilio 21220 / 400 "Invalid call state for
 * the requested operation" error.
 */
export function ErrCallNotInProgress(): ApiError {
  return new ApiError(21220, 'Invalid call state for the requested operation', STATUS_BAD_REQUEST);
}

/**
 * ErrTwimlParseFailure returns the Twilio 12100 / 400 "Document parse failure"
 * error.
 */
export function ErrTwimlParseFailure(): ApiError {
  return new ApiError(12100, 'Document parse failure', STATUS_BAD_REQUEST);
}

/**
 * ErrHttpRetrievalFailure returns the Twilio 11200 / 400 "HTTP retrieval
 * failure" error. `detail` is appended verbatim so operators can correlate the
 * 11200 response with the underlying transport-layer failure.
 */
export function ErrHttpRetrievalFailure(detail: string): ApiError {
  return new ApiError(11200, `HTTP retrieval failure: ${detail}`, STATUS_BAD_REQUEST);
}

/**
 * ErrPayloadTooLarge returns the Twilio 21617 / 413 "Body exceeds maximum
 * length" error. Emitted when a REST request body exceeds the 64KB cap.
 *
 * Twilio code 21617 is the closest semantic match in Twilio's published error
 * vocabulary for body-size violations — no dedicated 413 code exists.
 */
export function ErrPayloadTooLarge(): ApiError {
  return new ApiError(21617, 'Request body exceeds 64KB limit', STATUS_REQUEST_ENTITY_TOO_LARGE);
}

/**
 * ErrTooManyRequests returns the Twilio 20429 / 429 "Too Many Requests" error.
 * Used by the dial rate limiter.
 */
export function ErrTooManyRequests(): ApiError {
  return new ApiError(20429, 'Too Many Requests', STATUS_TOO_MANY_REQUESTS);
}
