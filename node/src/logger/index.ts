import pino, { type Logger } from 'pino';

export const rootLogger: Logger = pino({
  level: process.env.LOG_LEVEL ?? 'info',
  base: { service: 'audio-dock' },
});

export function createChildLogger(bindings: {
  component: string;
  callId?: string;
  streamSid?: string;
}): Logger {
  return rootLogger.child(bindings);
}
