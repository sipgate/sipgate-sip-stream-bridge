/**
 * Voice-URL pre-answer webhook client.
 *
 * POSTs Twilio-compatible call metadata to VOICE_URL before the 200 OK is sent
 * (pre-answer). The response must be TwiML containing <Connect><Stream url="..."/>;
 * extractStreamURL is called by the caller to pull the WS target URL.
 *
 * Isolated fetch client (separate keep-alive pool from status callbacks and
 * Url= fetches) so a slow VOICE_URL host cannot degrade other outbound paths.
 *
 * HTTPS is required for non-localhost URLs. http://localhost and
 * http://127.0.0.1 are permitted for local development — same bypass as
 * STATUS_CALLBACK_DEFAULT_URL.
 */

import { Agent as HttpAgent } from 'node:http';
import { Agent as HttpsAgent } from 'node:https';
import type { Config } from '../config/index.js';
import { sign } from './signer.js';

export interface VoiceParams {
  callSid: string;
  accountSid: string;
  from: string;
  to: string;
}

/** Builds the Twilio-compatible form body for the voice webhook POST. */
function buildFormBody(params: VoiceParams): string {
  const pairs: [string, string][] = [
    ['CallSid', params.callSid],
    ['AccountSid', params.accountSid],
    ['From', params.from],
    ['To', params.to],
    ['CallStatus', 'ringing'],
    ['Direction', 'inbound'],
    ['ApiVersion', '2010-04-01'],
    ['ForwardedFrom', ''],
  ];
  return pairs.map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`).join('&');
}

function isLocalhostHTTP(url: string): boolean {
  if (!url.startsWith('http://')) return false;
  const rest = url.slice('http://'.length);
  return rest.startsWith('localhost') || rest.startsWith('127.0.0.1');
}

function isAllowedVoiceURL(url: string): boolean {
  return url.startsWith('https://') || isLocalhostHTTP(url);
}

// Isolated HTTP/HTTPS keep-alive agents — one pool per scheme.
const httpsAgent = new HttpsAgent({ keepAlive: true, maxSockets: 4 });
const httpAgent = new HttpAgent({ keepAlive: true, maxSockets: 4 });

async function fetchOne(
  url: string,
  method: string,
  body: string,
  authToken: string,
  timeoutMs: number,
): Promise<Uint8Array> {
  if (!isAllowedVoiceURL(url)) {
    throw new Error(`voice-url: non-https URL not permitted: ${url}`);
  }

  const headers: Record<string, string> = {
    'Content-Type': 'application/x-www-form-urlencoded',
    Accept: 'application/xml, text/xml, */*',
  };

  if (authToken) {
    const formParams = new URLSearchParams(body);
    headers['X-Twilio-Signature'] = sign(authToken, url, formParams);
  }

  const agent = url.startsWith('https://') ? httpsAgent : httpAgent;

  const resp = await fetch(url, {
    method: method.toUpperCase(),
    headers,
    body: method.toUpperCase() === 'POST' ? body : undefined,
    signal: AbortSignal.timeout(timeoutMs),
    // @ts-expect-error — Node.js fetch supports agent via undici DispatchOptions
    dispatcher: undefined, // use keep-alive agents via globalThis.fetch override if needed
  });

  if (!resp.ok) {
    throw new Error(`voice-url: HTTP ${resp.status} from ${url}`);
  }

  const buf = await resp.arrayBuffer();
  return new Uint8Array(buf);
}

/**
 * Fetches the Voice-URL webhook and returns the raw TwiML response body.
 * Tries VOICE_FALLBACK_URL on HTTP 5xx / network error if configured.
 */
export async function fetchVoiceUrl(
  params: VoiceParams,
  cfg: Pick<
    Config,
    | 'VOICE_URL'
    | 'VOICE_METHOD'
    | 'VOICE_FALLBACK_URL'
    | 'VOICE_FALLBACK_METHOD'
    | 'VOICE_TIMEOUT_S'
    | 'AUTH_TOKEN'
  >,
): Promise<{ body: Uint8Array; urlUsed: 'primary' | 'fallback' }> {
  const timeoutMs = (cfg.VOICE_TIMEOUT_S ?? 5) * 1000;
  const formBody = buildFormBody(params);

  if (!cfg.VOICE_URL) {
    throw new Error('voice-url: VOICE_URL is not configured');
  }

  try {
    const body = await fetchOne(cfg.VOICE_URL, cfg.VOICE_METHOD ?? 'POST', formBody, cfg.AUTH_TOKEN, timeoutMs);
    return { body, urlUsed: 'primary' };
  } catch (primaryErr) {
    if (!cfg.VOICE_FALLBACK_URL) throw primaryErr;
    try {
      const body = await fetchOne(
        cfg.VOICE_FALLBACK_URL,
        cfg.VOICE_FALLBACK_METHOD ?? 'POST',
        formBody,
        cfg.AUTH_TOKEN,
        timeoutMs,
      );
      return { body, urlUsed: 'fallback' };
    } catch (fallbackErr) {
      throw new Error(
        `voice-url: all attempts failed — primary: ${(primaryErr as Error).message}; fallback: ${(fallbackErr as Error).message}`,
      );
    }
  }
}
