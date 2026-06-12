import { z } from 'zod';

const configSchema = z
  .object({
    // Required env vars
    SIP_USER: z.string().min(1, 'SIP_USER is required — your sipgate SIP-ID (e.g. e12345p0)'),
    SIP_PASSWORD: z.string().min(1, 'SIP_PASSWORD is required'),
    SIP_DOMAIN: z.string().min(1, 'SIP_DOMAIN is required (e.g. sipconnect.sipgate.de)'),
    SIP_REGISTRAR: z.string().min(1, 'SIP_REGISTRAR is required (e.g. sipconnect.sipgate.de)'),
    WS_TARGET_URL: z.string().url('WS_TARGET_URL must be a valid WebSocket URL'),

    // Optional with defaults
    RTP_PORT_MIN: z.coerce.number().int().min(1024).max(65534).default(10000),
    RTP_PORT_MAX: z.coerce.number().int().min(1025).max(65535).default(10099),
    SDP_CONTACT_IP: z.ipv4().optional(),
    SIP_EXPIRES: z.coerce.number().int().positive().default(120),
    SIP_OPTIONS_INTERVAL: z.coerce.number().int().positive().default(30),
    AUDIO_MODE: z.enum(['twilio', 'best']).default('twilio'),
    SRTP_ENABLED: z
      .string()
      .optional()
      .transform((val) => val === 'true' || val === '1')
      .pipe(z.boolean())
      .default(false),
    LOG_LEVEL: z.enum(['trace', 'debug', 'info', 'warn', 'error']).default('info'),
    HTTP_PORT: z.coerce.number().int().min(1).max(65535).default(9090),

    // ── v3 control plane ──────────────────────────────────────────────────────
    // REST Basic Auth password + X-Twilio-Signature HMAC key. Empty is allowed
    // for v2.1 parity (streaming-only); warn-worthy below 32 chars but not fatal.
    AUTH_TOKEN: z.string().default(''),
    // External base URL behind a reverse proxy — used to reconstruct the signed
    // URL for X-Twilio-Signature when the public URL differs from the bound one.
    PUBLIC_BASE_URL: z.string().url().optional(),
    // Explicit port for outbound INVITE Request-URI; 0 = DNS/default.
    SIP_OUTBOUND_TARGET_PORT: z.coerce.number().int().min(0).max(65535).default(0),
    // SIP listener bind address host:port. Drives the inbound socket bind AND the
    // Via/Contact port. Default 0.0.0.0:5060 (production). The sipp e2e harness sets
    // 127.0.0.1:5070 so the bridge coexists with a test-registrar stub on :5060.
    SIP_LISTEN_ADDR: z
      .string()
      .regex(/^[^:]+:\d{1,5}$/, 'must be host:port')
      .default('0.0.0.0:5060'),

    // ── v3 status callbacks ───────────────────────────────────────────────────
    // Operator-supplied default StatusCallback installed on every inbound call.
    // Empty = disabled. Trusted (bypasses the SSRF guard).
    STATUS_CALLBACK_DEFAULT_URL: z
      .string()
      .url()
      .refine((u) => /^https?:\/\//i.test(u), 'must be http(s)://')
      .optional(),
    STATUS_CALLBACK_DEFAULT_METHOD: z.enum(['POST', 'GET']).default('POST'),
    STATUS_CALLBACK_DEFAULT_EVENTS: z.string().default('initiated,ringing,answered,completed'),

    // ── v3 <Dial> / B2BUA forwarding ──────────────────────────────────────────
    // Toll-fraud allow-list of E.164 prefixes (CSV). Empty = DENY ALL (default-deny).
    DIAL_ALLOWED_PREFIXES: z.string().default(''),
    // Fallback caller-ID when TwiML <Dial callerId> is absent.
    DIAL_DEFAULT_CALLER_ID: z.string().optional(),
    // E.164 country code without "+" (e.g. "49") for trunk caller-ID normalization.
    DIAL_CALLER_ID_COUNTRY_CODE: z.string().regex(/^[0-9]+$/, 'digits only, no +').optional(),
    DIAL_RING_TIMEOUT_S: z.coerce.number().int().min(5).max(600).default(30),
    DIAL_MAX_PER_SESSION: z.coerce.number().int().min(1).default(3),
    DIAL_MAX_PER_MINUTE: z.coerce.number().int().min(1).default(60),
  })
  .refine((data) => data.RTP_PORT_MIN < data.RTP_PORT_MAX, {
    message: 'RTP_PORT_MIN must be less than RTP_PORT_MAX',
    path: ['RTP_PORT_MIN'],
  })
  .refine(
    (data) =>
      data.STATUS_CALLBACK_DEFAULT_EVENTS.split(',')
        .map((e) => e.trim())
        .filter((e) => e.length > 0)
        .every((e) =>
          [
            'initiated', 'ringing', 'answered', 'in-progress',
            'completed', 'busy', 'failed', 'no-answer', 'canceled',
          ].includes(e),
        ),
    {
      message: 'STATUS_CALLBACK_DEFAULT_EVENTS must be a CSV of valid call-status events',
      path: ['STATUS_CALLBACK_DEFAULT_EVENTS'],
    },
  )
  .refine(
    (data) =>
      data.DIAL_ALLOWED_PREFIXES.split(',')
        .map((p) => p.trim())
        .filter((p) => p.length > 0)
        .every((p) => /^\+?[0-9]*$/.test(p)),
    {
      message: 'DIAL_ALLOWED_PREFIXES entries must match ^\\+?[0-9]*$ (e.g. +49, 0, or + for all)',
      path: ['DIAL_ALLOWED_PREFIXES'],
    },
  );

export type Config = z.infer<typeof configSchema>;

function loadConfig(): Config {
  const result = configSchema.safeParse(process.env);
  if (!result.success) {
    console.error(
      JSON.stringify({
        level: 'error',
        msg: 'Configuration validation failed',
        errors: result.error.flatten().fieldErrors,
      }),
    );
    process.exit(1);
  }
  return result.data;
}

export const config: Config = loadConfig();
