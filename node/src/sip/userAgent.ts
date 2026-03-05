import crypto from 'node:crypto';
import dgram from 'node:dgram';
import type { RemoteInfo } from 'node:dgram';
import dns from 'node:dns/promises';
import os from 'node:os';

import type { Logger } from 'pino';

import type { Config } from '../config/index.js';

export interface SipCallbacks {
  onInvite?: (raw: string, rinfo: RemoteInfo) => void;
  onAck?: (raw: string, rinfo: RemoteInfo) => void;
  onBye?: (raw: string, rinfo: RemoteInfo) => void;
  onCancel?: (raw: string, rinfo: RemoteInfo) => void;
}

export interface SipHandle {
  stop(): void;
  /** Send a raw SIP message buffer to a remote address — used by CallManager to send INVITE responses and BYE */
  sendRaw(buf: Buffer, port: number, host: string): void;
  /** Send REGISTER with Expires:0 and Contact:* to deregister all bindings */
  unregister(): Promise<void>;
}

const USER_AGENT = 'audio-dock/0.1.0';

// ── helpers ──────────────────────────────────────────────────────────────────

function randomHex(n: number): string {
  return crypto.randomBytes(n).toString('hex');
}

function md5(s: string): string {
  return crypto.createHash('md5').update(s).digest('hex');
}

function getLocalIp(): string {
  for (const ifaces of Object.values(os.networkInterfaces())) {
    for (const iface of ifaces ?? []) {
      if (!iface.internal && iface.family === 'IPv4') return iface.address;
    }
  }
  return '127.0.0.1';
}

function parseAuthChallenge(header: string): Record<string, string> {
  const result: Record<string, string> = {};
  let m: RegExpExecArray | null;
  const re = /(\w+)="([^"]*)"/g;
  while ((m = re.exec(header)) !== null) result[m[1]] = m[2];
  return result;
}

function buildRegister(p: {
  user: string;
  domain: string;
  registrar: string;
  localIp: string;
  localPort: number;
  callId: string;
  fromTag: string;
  seq: number;
  expires: number;
  branch: string;
  auth?: string;
}): string {
  const sipUri = `sip:${p.user}@${p.domain}`;
  const registrarUri = `sip:${p.registrar}`;
  const contactUri = `sip:${p.user}@${p.localIp}:${p.localPort}`;
  const lines = [
    `REGISTER ${registrarUri} SIP/2.0`,
    `Via: SIP/2.0/UDP ${p.localIp}:${p.localPort};branch=${p.branch};rport`,
    `Max-Forwards: 70`,
    `From: <${sipUri}>;tag=${p.fromTag}`,
    `To: <${sipUri}>`,
    `Call-ID: ${p.callId}`,
    `CSeq: ${p.seq} REGISTER`,
    `Contact: <${contactUri}>`,
    `Expires: ${p.expires}`,
    `User-Agent: ${USER_AGENT}`,
  ];
  if (p.auth) lines.push(`Authorization: ${p.auth}`);
  lines.push('Content-Length: 0', '');
  return lines.join('\r\n') + '\r\n';
}

function parseStatusLine(raw: string): { status: number; reason: string } {
  const m = raw.split('\r\n')[0].match(/SIP\/2\.0 (\d+) (.*)/);
  return m ? { status: +m[1], reason: m[2] } : { status: 0, reason: 'unparseable' };
}

function getHeader(raw: string, name: string): string | null {
  const m = raw.match(new RegExp(`^${name}:\\s*(.+)$`, 'im'));
  return m ? m[1].trim() : null;
}

// ── pure helper — exported for unit tests ─────────────────────────────────────

export function applyOptionsResponse(
  consecutiveFailures: number,
  status: number,
  isError: boolean,
): { newCount: number; triggerRegister: boolean } {
  const threshold = 2;
  const isAuth = status === 401 || status === 407;
  const isFailure = isError || status === 404 || status >= 500;
  if (isAuth) return { newCount: 0, triggerRegister: false };
  if (isFailure) {
    const next = consecutiveFailures + 1;
    if (next >= threshold) return { newCount: 0, triggerRegister: true };
    return { newCount: next, triggerRegister: false };
  }
  return { newCount: 0, triggerRegister: false };
}

// ── main export ───────────────────────────────────────────────────────────────

export async function createSipUserAgent(
  config: Config,
  log: Logger,
  callbacks?: SipCallbacks,
): Promise<SipHandle> {
  const registrar = config.SIP_REGISTRAR; // e.g. sipconnect.sipgate.de
  const registrarPort = 5060;

  const [registrarIp] = await dns.resolve4(registrar);
  const localIp = config.SDP_CONTACT_IP ?? getLocalIp();
  const localPort = 5060;
  const callId = `${randomHex(10)}@audio-dock`;
  const fromTag = randomHex(6);
  let cseq = 1;
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;
  let consecutiveFailures = 0;
  let pingTimer: ReturnType<typeof setTimeout> | null = null;
  let optionsTimer: ReturnType<typeof setInterval> | null = null;
  let settled = false;

  const socket = dgram.createSocket('udp4');

  function sendRegister(authHeader?: string): void {
    const branch = `z9hG4bK${randomHex(6)}`;
    const msg = buildRegister({
      user: config.SIP_USER,
      domain: config.SIP_DOMAIN,
      registrar,
      localIp,
      localPort,
      callId,
      fromTag,
      seq: cseq++,
      expires: config.SIP_EXPIRES,
      branch,
      auth: authHeader,
    });
    const buf = Buffer.from(msg);
    socket.send(buf, registrarPort, registrarIp, (err) => {
      if (err) log.error({ err, event: 'sip_send_error' }, 'Failed to send REGISTER');
    });
  }

  function buildOptions(seq: number, branch: string): string {
    const sipUri = `sip:${config.SIP_USER}@${config.SIP_DOMAIN}`;
    const registrarUri = `sip:${registrar}`;
    const lines = [
      `OPTIONS ${registrarUri} SIP/2.0`,
      `Via: SIP/2.0/UDP ${localIp}:${localPort};branch=${branch};rport`,
      'Max-Forwards: 70',
      `From: <${sipUri}>;tag=${fromTag}`,
      `To: <${sipUri}>`,
      `Call-ID: ${callId}`,
      `CSeq: ${seq} OPTIONS`,
      `User-Agent: ${USER_AGENT}`,
      'Content-Length: 0',
      '',
    ];
    return lines.join('\r\n') + '\r\n';
  }

  function handleOptionsResponse(status: number, err: Error | null): void {
    const result = applyOptionsResponse(consecutiveFailures, status, err !== null);
    consecutiveFailures = result.newCount;
    if (result.triggerRegister) {
      log.warn({ event: 'options_reregister' }, 'OPTIONS keepalive: 2 consecutive failures — re-registering');
      sendRegister();
    } else if (err !== null || status === 404 || status >= 500) {
      log.warn({ event: 'options_failure', consecutiveFailures }, 'OPTIONS keepalive failure');
    } else if (status === 401 || status === 407) {
      log.debug({ event: 'options_auth', status }, 'OPTIONS 401/407 — server reachable');
    } else {
      log.debug({ event: 'options_ok' }, 'OPTIONS keepalive success');
    }
  }

  log.info({ event: 'ua_starting', registrar, localIp, localPort }, 'Starting SIP registrar (UDP)');

  return new Promise<SipHandle>((resolve, reject) => {
    socket.on('error', (err) => {
      log.error({ err, event: 'sip_udp_error' }, 'SIP UDP socket error');
      if (!settled) {
        settled = true;
        reject(err);
      }
    });

    socket.on('message', (buf, rinfo) => {
      const raw = buf.toString();
      const firstLine = raw.split('\r\n')[0];

      if (firstLine.startsWith('SIP/2.0')) {
        // ---- CSeq routing: OPTIONS responses go to handleOptionsResponse ----
        const cseqVal = getHeader(raw, 'CSeq') ?? '';
        if (cseqVal.includes('OPTIONS')) {
          if (pingTimer !== null) { clearTimeout(pingTimer); pingTimer = null; }
          const { status } = parseStatusLine(raw);
          handleOptionsResponse(status, null);
          return;
        }
        // else: fall through to existing REGISTER response handling
        const { status, reason } = parseStatusLine(raw);

        if (status === 401 || status === 407) {
          const challengeHeader = getHeader(raw, status === 401 ? 'WWW-Authenticate' : 'Proxy-Authenticate');
          if (!challengeHeader) {
            log.error({ event: 'auth_missing', status }, `Got ${status} without auth challenge header`);
            if (!settled) {
              settled = true;
              reject(new Error(`SIP ${status} without challenge`));
            }
            return;
          }
          const params = parseAuthChallenge(challengeHeader);
          const registrarUri = `sip:${registrar}`;
          const ha1 = md5(`${config.SIP_USER}:${params['realm']}:${config.SIP_PASSWORD}`);
          const ha2 = md5(`REGISTER:${registrarUri}`);
          const response = md5(`${ha1}:${params['nonce']}:${ha2}`);
          const auth = `Digest username="${config.SIP_USER}", realm="${params['realm']}", nonce="${params['nonce']}", uri="${registrarUri}", response="${response}", algorithm=MD5`;
          log.debug({ event: 'auth_challenge', status }, `Responding to ${status} auth challenge`);
          sendRegister(auth);
        } else if (status === 200) {
          log.info({ event: 'sip_registered', expires: config.SIP_EXPIRES }, 'SIP 200 OK — registration confirmed');
          const refreshMs = Math.floor(config.SIP_EXPIRES * 0.9) * 1000;
          if (refreshTimer) clearTimeout(refreshTimer);
          refreshTimer = setTimeout(() => {
            log.debug({ event: 'sip_reregister' }, 'Re-registering before expiry');
            sendRegister();
          }, refreshMs);
          if (optionsTimer) clearInterval(optionsTimer);
          optionsTimer = setInterval(() => {
            const branch = `z9hG4bK${randomHex(6)}`;
            const optBuf = Buffer.from(buildOptions(cseq++, branch));
            socket.send(optBuf, registrarPort, registrarIp, (err) => {
              if (err) {
                handleOptionsResponse(0, err);
              }
              // else: wait for response via socket.on('message')
            });
            pingTimer = setTimeout(() => {
              pingTimer = null;
              handleOptionsResponse(0, new Error('OPTIONS timeout'));
            }, 5000);
          }, config.SIP_OPTIONS_INTERVAL * 1000);
          if (!settled) {
            settled = true;
            resolve({
              stop() {
                if (refreshTimer) clearTimeout(refreshTimer);
                if (optionsTimer) clearInterval(optionsTimer);
                if (pingTimer) clearTimeout(pingTimer);
                socket.close();
              },
              sendRaw(sendBuf: Buffer, port: number, host: string): void {
                socket.send(sendBuf, port, host, (err) => {
                  if (err) log.error({ err, event: 'sip_send_raw_error' }, 'sendRaw failed');
                });
              },
              unregister(): Promise<void> {
                const branch = `z9hG4bK${randomHex(6)}`;
                const sipUri = `sip:${config.SIP_USER}@${config.SIP_DOMAIN}`;
                const registrarUri = `sip:${registrar}`;
                const msg = [
                  `REGISTER ${registrarUri} SIP/2.0`,
                  `Via: SIP/2.0/UDP ${localIp}:${localPort};branch=${branch};rport`,
                  'Max-Forwards: 70',
                  `From: <${sipUri}>;tag=${fromTag}`,
                  `To: <${sipUri}>`,
                  `Call-ID: ${callId}`,
                  `CSeq: ${cseq++} REGISTER`,
                  'Contact: *',
                  'Expires: 0',
                  'Content-Length: 0',
                  '',
                ].join('\r\n') + '\r\n';
                socket.send(Buffer.from(msg), registrarPort, registrarIp, (err) => {
                  if (err) log.error({ err, event: 'sip_unregister_error' }, 'unregister send failed');
                });
                return Promise.resolve();
              },
            });
          }
        } else {
          log.error({ event: 'sip_register_failed', status, reason }, `SIP registration rejected: ${status} ${reason}`);
          if (!settled) {
            settled = true;
            reject(new Error(`SIP ${status} ${reason}`));
          }
        }
      } else if (firstLine.startsWith('INVITE ')) {
        callbacks?.onInvite?.(raw, rinfo);
      } else if (firstLine.startsWith('ACK ')) {
        callbacks?.onAck?.(raw, rinfo);
      } else if (firstLine.startsWith('BYE ')) {
        callbacks?.onBye?.(raw, rinfo);
      } else if (firstLine.startsWith('CANCEL ')) {
        callbacks?.onCancel?.(raw, rinfo);
      } else if (firstLine.startsWith('OPTIONS ')) {
        // Respond 200 OK to OPTIONS keepalive (defends against sipgate liveness probes)
        const via = getHeader(raw, 'Via') ?? '';
        const from = getHeader(raw, 'From') ?? '';
        const to = getHeader(raw, 'To') ?? '';
        const callIdVal = getHeader(raw, 'Call-ID') ?? '';
        const cseqVal = getHeader(raw, 'CSeq') ?? '';
        const response = [
          'SIP/2.0 200 OK',
          `Via: ${via}`,
          `From: ${from}`,
          `To: ${to}`,
          `Call-ID: ${callIdVal}`,
          `CSeq: ${cseqVal}`,
          `User-Agent: ${USER_AGENT}`,
          'Content-Length: 0',
          '',
        ].join('\r\n') + '\r\n';
        socket.send(Buffer.from(response), rinfo.port, rinfo.address);
      }
      // other methods (REGISTER requests from proxies etc.) silently ignored
    });

    socket.bind(localPort, () => {
      sendRegister();
    });
  });
}
