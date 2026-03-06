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
    LOG_LEVEL: z.enum(['trace', 'debug', 'info', 'warn', 'error']).default('info'),
    HTTP_PORT: z.coerce.number().int().min(1).max(65535).default(9090),
  })
  .refine((data) => data.RTP_PORT_MIN < data.RTP_PORT_MAX, {
    message: 'RTP_PORT_MIN must be less than RTP_PORT_MAX',
    path: ['RTP_PORT_MIN'],
  });

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
