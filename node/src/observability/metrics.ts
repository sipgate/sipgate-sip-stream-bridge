/**
 * Metrics — simple in-memory counters/gauges for Prometheus scraping.
 *
 * Implements the 5 metrics defined in OBS-03:
 *   active_calls_total          gauge   — live sessions in CallManager
 *   sip_registration_status     gauge   — 1 registered / 0 not
 *   rtp_packets_received_total  counter — inbound RTP packets from callers
 *   rtp_packets_sent_total      counter — outbound RTP packets to callers
 *   ws_reconnect_attempts_total counter — WS reconnect attempts
 *
 * No external dependencies — outputs standard Prometheus text exposition format.
 */

export class Metrics {
  private activeCalls = 0;
  private sipRegistered = 0;
  private rtpRxTotal = 0;
  private rtpTxTotal = 0;
  private wsReconnectsTotal = 0;

  incActiveCalls(): void  { this.activeCalls++; }
  decActiveCalls(): void  { if (this.activeCalls > 0) this.activeCalls--; }
  setSipRegistered(v: boolean): void { this.sipRegistered = v ? 1 : 0; }
  incRtpRx(): void        { this.rtpRxTotal++; }
  incRtpTx(): void        { this.rtpTxTotal++; }
  incWsReconnect(): void  { this.wsReconnectsTotal++; }

  /** JSON body for GET /health (OBS-02) */
  getHealth(): { registered: boolean; activeCalls: number } {
    return { registered: this.sipRegistered === 1, activeCalls: this.activeCalls };
  }

  /** Prometheus text exposition for GET /metrics (OBS-03) */
  getPrometheus(): string {
    const lines: string[] = [];
    const emit = (name: string, type: string, help: string, value: number): void => {
      lines.push(`# HELP ${name} ${help}`);
      lines.push(`# TYPE ${name} ${type}`);
      lines.push(`${name} ${value}`);
    };
    emit('active_calls_total',          'gauge',   'Number of currently active calls',                             this.activeCalls);
    emit('sip_registration_status',     'gauge',   '1 if registered with SIP registrar, 0 if not',                this.sipRegistered);
    emit('rtp_packets_received_total',  'counter', 'Total RTP audio packets received from callers',               this.rtpRxTotal);
    emit('rtp_packets_sent_total',      'counter', 'Total RTP audio packets sent to callers (incl. silence)',     this.rtpTxTotal);
    emit('ws_reconnect_attempts_total', 'counter', 'Total WebSocket reconnect attempts across all calls',         this.wsReconnectsTotal);
    return lines.join('\n') + '\n';
  }
}
