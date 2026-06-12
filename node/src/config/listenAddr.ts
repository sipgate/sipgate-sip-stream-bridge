/**
 * Pure SIP_LISTEN_ADDR parsing — kept separate from config/index.ts so importing
 * it does NOT trigger config/index.ts's load-time validation (which process.exits
 * on missing env). Side-effect-free; safe to import from any module + tests.
 */

/** Split SIP_LISTEN_ADDR ("host:port") into its bind host + port. */
export function listenParts(addr: string): { host: string; port: number } {
  const i = addr.lastIndexOf(':');
  return { host: addr.slice(0, i), port: parseInt(addr.slice(i + 1), 10) };
}
