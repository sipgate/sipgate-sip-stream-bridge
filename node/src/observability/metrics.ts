/**
 * Metrics — simple in-memory counters/gauges/histograms for Prometheus scraping.
 *
 * Port of go/internal/observability/metrics.go.
 *
 * Original 5 metrics (OBS-03):
 *   active_calls_total          gauge   — live sessions in CallManager
 *   sip_registration_status     gauge   — 1 registered / 0 not
 *   rtp_packets_received_total  counter — inbound RTP packets from callers
 *   rtp_packets_sent_total      counter — outbound RTP packets to callers
 *   ws_reconnect_attempts_total counter — WS reconnect attempts
 *
 * v3 metrics use BOUNDED-CARDINALITY label sets only — label values are a
 * fixed enum (NEVER CallSid/AccountSid/phone/URL). Labeled counters and
 * histograms are stored in Maps keyed by the label tuple and emitted as
 * Prometheus text with `name{label="value"} N` syntax.
 *
 * No external dependencies — outputs standard Prometheus text exposition format.
 */

// ── Bounded-cardinality label enums (confirmed against metrics.go) ──

/** route ∈ {list_calls, get_call, modify_call, health, metrics, unknown} */
export type ApiRoute = 'list_calls' | 'get_call' | 'modify_call' | 'health' | 'metrics' | 'unknown';
/** method ∈ {GET, POST, PUT, DELETE} */
export type ApiMethod = 'GET' | 'POST' | 'PUT' | 'DELETE';
/** status ∈ {2xx, 4xx, 5xx} */
export type StatusBucket = '2xx' | '4xx' | '5xx';
/** twiml_modify kind ∈ {twiml, url, status_completed} */
export type TwimlModifyKind = 'twiml' | 'url' | 'status_completed';
/** twiml_modify outcome ∈ {ok, parse_error, fetch_error, invalid_params, terminated, hangup} */
export type TwimlModifyOutcome =
  | 'ok'
  | 'parse_error'
  | 'fetch_error'
  | 'invalid_params'
  | 'terminated'
  | 'hangup';
/** forward_failed reason enum */
export type ForwardReason =
  | 'busy'
  | 'no_answer'
  | 'rejected'
  | 'unreachable'
  | 'codec_mismatch'
  | 'toll_fraud'
  | 'rate_limit'
  | 'caller_id_rejected'
  | 'auth_failed'
  | 'trunk_5xx'
  | 'timeout'
  | 'error';
/** forward_duration outcome ∈ {answered, no_answer, busy, error} */
export type ForwardOutcome = 'answered' | 'no_answer' | 'busy' | 'error';
/** auth_challenge_kind kind ∈ {401, 407} */
export type AuthChallengeKind = '401' | '407';
/** status_callback event vocabulary (9 values) */
export type StatusCallbackEvent =
  | 'initiated'
  | 'ringing'
  | 'answered'
  | 'in-progress'
  | 'completed'
  | 'busy'
  | 'failed'
  | 'no-answer'
  | 'canceled';
/** status_callback_failures reason enum (7 values) */
export type StatusCallbackReason =
  | 'timeout'
  | '4xx'
  | '5xx'
  | 'connect_error'
  | 'exhausted_retries'
  | 'ssrf_rejected'
  | 'queue_full';

// ── Histogram buckets ──

/** Prometheus client default buckets (prometheus.DefBuckets). */
const DEF_BUCKETS: readonly number[] = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10];

/** forward_duration_seconds buckets — long-tail call durations (seconds). */
const FORWARD_DURATION_BUCKETS: readonly number[] = [
  0.1, 0.5, 1, 2, 5, 10, 30, 60, 300, 1800, 7200, 14400,
];

/**
 * A labeled counter: maps a serialized label tuple to a running count, and
 * remembers the ordered label keys so it can re-emit the original label set.
 */
class LabeledCounter {
  private readonly values = new Map<string, { labels: Record<string, string>; count: number }>();

  constructor(
    private readonly name: string,
    private readonly type: 'counter' | 'gauge',
    private readonly help: string,
    private readonly labelKeys: readonly string[],
  ) {}

  inc(labels: Record<string, string>, delta = 1): void {
    const key = this.keyFor(labels);
    const existing = this.values.get(key);
    if (existing) {
      existing.count += delta;
    } else {
      this.values.set(key, { labels: { ...labels }, count: delta });
    }
  }

  private keyFor(labels: Record<string, string>): string {
    return this.labelKeys.map((k) => `${k}=${labels[k] ?? ''}`).join(',');
  }

  emit(lines: string[]): void {
    lines.push(`# HELP ${this.name} ${this.help}`);
    lines.push(`# TYPE ${this.name} ${this.type}`);
    for (const { labels, count } of this.values.values()) {
      lines.push(`${this.name}{${formatLabels(this.labelKeys, labels)}} ${count}`);
    }
  }
}

/**
 * A labeled histogram: per label tuple it tracks cumulative bucket counts,
 * sum, and total count. Emits the standard _bucket / _sum / _count lines.
 */
class LabeledHistogram {
  private readonly series = new Map<
    string,
    { labels: Record<string, string>; buckets: number[]; sum: number; count: number }
  >();

  constructor(
    private readonly name: string,
    private readonly help: string,
    private readonly labelKeys: readonly string[],
    private readonly buckets: readonly number[],
  ) {}

  observe(labels: Record<string, string>, value: number): void {
    const key = this.keyFor(labels);
    let s = this.series.get(key);
    if (!s) {
      s = { labels: { ...labels }, buckets: new Array<number>(this.buckets.length).fill(0), sum: 0, count: 0 };
      this.series.set(key, s);
    }
    s.sum += value;
    s.count += 1;
    for (let i = 0; i < this.buckets.length; i++) {
      const bound = this.buckets[i];
      if (bound !== undefined && value <= bound) {
        s.buckets[i] = (s.buckets[i] ?? 0) + 1;
      }
    }
  }

  private keyFor(labels: Record<string, string>): string {
    return this.labelKeys.map((k) => `${k}=${labels[k] ?? ''}`).join(',');
  }

  emit(lines: string[]): void {
    lines.push(`# HELP ${this.name} ${this.help}`);
    lines.push(`# TYPE ${this.name} histogram`);
    for (const s of this.series.values()) {
      const base = formatLabels(this.labelKeys, s.labels);
      for (let i = 0; i < this.buckets.length; i++) {
        const le = this.buckets[i];
        const sep = base.length > 0 ? ',' : '';
        lines.push(`${this.name}_bucket{${base}${sep}le="${le}"} ${s.buckets[i] ?? 0}`);
      }
      const sep = base.length > 0 ? ',' : '';
      lines.push(`${this.name}_bucket{${base}${sep}le="+Inf"} ${s.count}`);
      lines.push(`${this.name}_sum{${base}} ${s.sum}`);
      lines.push(`${this.name}_count{${base}} ${s.count}`);
    }
  }
}

/** Format a label set as Prometheus `k="v",k2="v2"` using a stable key order. */
function formatLabels(labelKeys: readonly string[], labels: Record<string, string>): string {
  return labelKeys.map((k) => `${k}="${labels[k] ?? ''}"`).join(',');
}

// ── Bucketer helpers (ported 1:1 from metrics.go) ──

/**
 * BucketStatus maps an HTTP status code to a bounded-cardinality label.
 *   - code < 400 (incl. <100 implicit-200 and 3xx) → "2xx"
 *   - 400..499 → "4xx"
 *   - >= 500 (incl. >= 600) → "5xx"
 */
export function bucketStatus(code: number): StatusBucket {
  if (code < 100) return '2xx';
  if (code < 400) return '2xx';
  if (code < 500) return '4xx';
  return '5xx';
}

/** Case-insensitive substring test. */
function containsCI(s: string, substr: string): boolean {
  return s.toLowerCase().includes(substr.toLowerCase());
}

/** Normalize an error-ish input into the message string used for matching. */
function errMessage(err: unknown): string | null {
  if (err === null || err === undefined) return null;
  if (typeof err === 'string') return err;
  if (err instanceof Error) return err.message;
  return String(err);
}

/**
 * BucketForwardReason maps a forwarder error to the canonical reason string
 * used in forward_failed_total{reason}. Returns '' (caller falls back to the
 * "error" bucket) when unclassified, and '' for nil/empty input (success path).
 *
 * Match order matters: specific patterns precede generic ones (guardrails,
 * caller-id, then SIP status codes, auth, 5xx, timeout, unreachable).
 */
export function bucketForwardReason(err: unknown): ForwardReason | '' {
  const msg = errMessage(err);
  if (msg === null || msg === '') return '';
  if (containsCI(msg, 'ErrTollFraudBlocked') || containsCI(msg, 'toll-fraud') || containsCI(msg, 'toll_fraud')) {
    return 'toll_fraud';
  }
  if (
    containsCI(msg, 'ErrSessionRateLimit') ||
    containsCI(msg, 'ErrGlobalRateLimit') ||
    containsCI(msg, 'rate limit') ||
    containsCI(msg, 'rate-limit') ||
    containsCI(msg, 'rate_limit')
  ) {
    return 'rate_limit';
  }
  if (
    containsCI(msg, '13214') ||
    containsCI(msg, 'caller-id') ||
    containsCI(msg, 'caller_id') ||
    containsCI(msg, 'callerid')
  ) {
    return 'caller_id_rejected';
  }
  if (containsCI(msg, '486') || containsCI(msg, 'Busy')) return 'busy';
  if (containsCI(msg, '603') || containsCI(msg, 'Decline')) return 'rejected';
  if (
    containsCI(msg, '408') ||
    containsCI(msg, '480') ||
    containsCI(msg, 'no-answer') ||
    containsCI(msg, 'no answer') ||
    containsCI(msg, 'no_answer')
  ) {
    return 'no_answer';
  }
  if (containsCI(msg, 'codec')) return 'codec_mismatch';
  if (containsCI(msg, '401') || containsCI(msg, '407') || containsCI(msg, 'auth')) return 'auth_failed';
  if (
    containsCI(msg, '5xx') ||
    containsCI(msg, '500') ||
    containsCI(msg, '502') ||
    containsCI(msg, '503') ||
    containsCI(msg, '504')
  ) {
    return 'trunk_5xx';
  }
  if (containsCI(msg, 'timeout') || containsCI(msg, 'deadline')) return 'timeout';
  if (containsCI(msg, 'unreachable') || containsCI(msg, 'no route') || containsCI(msg, 'DNS')) {
    return 'unreachable';
  }
  return '';
}

/**
 * BucketOutcome maps a sip-layer DialResult status onto the bounded
 * forward_duration_seconds outcome label.
 *   answered → answered ; no-answer → no_answer ; busy → busy ; default → error
 */
export function bucketOutcome(status: string): ForwardOutcome {
  switch (status) {
    case 'answered':
      return 'answered';
    case 'no-answer':
      return 'no_answer';
    case 'busy':
      return 'busy';
    default:
      return 'error';
  }
}

/**
 * BucketStatusCallbackReason maps a status-callback delivery outcome to the
 * canonical reason bucket used in status_callback_failures_total{reason}.
 *
 * Returns '' when the outcome is success (no error AND 2xx) OR genuinely
 * unclassified — callers MUST treat '' as "do not record a failure".
 */
export function bucketStatusCallbackReason(err: unknown, statusCode: number): StatusCallbackReason | '' {
  const msg = errMessage(err);
  if (msg === null && statusCode >= 200 && statusCode < 300) {
    return '';
  }

  // (1) Typed-sentinel substring match.
  if (msg !== null) {
    if (containsCI(msg, 'queue full') || containsCI(msg, 'queue_full')) return 'queue_full';
    if (containsCI(msg, 'retries exhausted') || containsCI(msg, 'exhausted_retries')) return 'exhausted_retries';
    if (containsCI(msg, 'ssrf') || containsCI(msg, 'blocked address space') || containsCI(msg, 'blocked ip')) {
      return 'ssrf_rejected';
    }
    if (containsCI(msg, 'timeout') || containsCI(msg, 'deadline exceeded')) return 'timeout';
    if (
      containsCI(msg, 'connection refused') ||
      containsCI(msg, 'no such host') ||
      containsCI(msg, 'dns lookup') ||
      containsCI(msg, 'dial tcp') ||
      containsCI(msg, 'tls handshake')
    ) {
      return 'connect_error';
    }
  }

  // (2) HTTP status code ranges.
  if (statusCode >= 500) return '5xx';
  if (statusCode >= 400) return '4xx';

  // (3) Generic err with no specific match → connect_error.
  if (msg !== null) return 'connect_error';
  return '';
}

export class Metrics {
  // ── original 5 ──
  private activeCalls = 0;
  private sipRegistered = 0;
  private rtpRxTotal = 0;
  private rtpTxTotal = 0;
  private wsReconnectsTotal = 0;

  // ── v3 unlabeled scalars ──
  private forwardAttemptsTotal = 0;
  private forwardSuccessTotal = 0;
  private rtpPortPoolInUse = 0;
  private rtpPortPoolSize = 0;
  private rtpPortAcquireFailuresTotal = 0;

  // ── v3 labeled collectors ──
  private readonly twimlParseErrors = new LabeledCounter(
    'twiml_parse_errors_total',
    'counter',
    'Total TwiML document parse failures, bucketed by Twilio error-code (12100|13xxx|21218|21220).',
    ['code'],
  );
  private readonly apiRequests = new LabeledCounter(
    'api_requests_total',
    'counter',
    'Total HTTP requests to the Twilio-compatible REST API, by route, method, and bucketed status.',
    ['route', 'method', 'status'],
  );
  private readonly apiRequestDuration = new LabeledHistogram(
    'api_request_duration_seconds',
    'Latency of Twilio-compatible REST API handlers, by route.',
    ['route'],
    DEF_BUCKETS,
  );
  private readonly twimlModify = new LabeledCounter(
    'twiml_modify_total',
    'counter',
    'Total TwiML modify-call handler invocations, bucketed by body kind and outcome.',
    ['kind', 'outcome'],
  );
  private readonly forwardFailed = new LabeledCounter(
    'sipgate_bridge_forward_failed_total',
    'counter',
    'Outbound dials that failed, bucketed by reason.',
    ['reason'],
  );
  private readonly forwardDuration = new LabeledHistogram(
    'sipgate_bridge_forward_duration_seconds',
    'End-to-end <Dial> duration in seconds, bucketed by outcome.',
    ['outcome'],
    FORWARD_DURATION_BUCKETS,
  );
  private readonly authChallengeKind = new LabeledCounter(
    'sipgate_bridge_auth_challenge_kind_total',
    'counter',
    'Outbound-INVITE digest challenge type observed (401 UAS vs 407 proxy).',
    ['kind'],
  );
  private readonly statusCallbackAttempts = new LabeledCounter(
    'sipgate_bridge_status_callback_attempts_total',
    'counter',
    'Outbound Twilio-shape status callback POST attempts, bucketed by event vocabulary.',
    ['event'],
  );
  private readonly statusCallbackFailures = new LabeledCounter(
    'sipgate_bridge_status_callback_failures_total',
    'counter',
    'Outbound status callback failures, bucketed by reason.',
    ['reason'],
  );

  // ── original 5 mutators ──
  incActiveCalls(): void {
    this.activeCalls++;
  }
  decActiveCalls(): void {
    if (this.activeCalls > 0) this.activeCalls--;
  }
  setSipRegistered(v: boolean): void {
    this.sipRegistered = v ? 1 : 0;
  }
  incRtpRx(): void {
    this.rtpRxTotal++;
  }
  incRtpTx(): void {
    this.rtpTxTotal++;
  }
  incWsReconnect(): void {
    this.wsReconnectsTotal++;
  }

  // ── v3 mutators ──
  incTwimlParseError(code: string): void {
    this.twimlParseErrors.inc({ code });
  }
  incApiRequest(route: ApiRoute, method: ApiMethod, status: StatusBucket): void {
    this.apiRequests.inc({ route, method, status });
  }
  observeApiRequestDuration(route: ApiRoute, seconds: number): void {
    this.apiRequestDuration.observe({ route }, seconds);
  }
  incTwimlModify(kind: TwimlModifyKind, outcome: TwimlModifyOutcome): void {
    this.twimlModify.inc({ kind, outcome });
  }
  incForwardAttempts(): void {
    this.forwardAttemptsTotal++;
  }
  incForwardSuccess(): void {
    this.forwardSuccessTotal++;
  }
  incForwardFailed(reason: ForwardReason): void {
    this.forwardFailed.inc({ reason });
  }
  observeForwardDuration(outcome: ForwardOutcome, seconds: number): void {
    this.forwardDuration.observe({ outcome }, seconds);
  }
  incAuthChallengeKind(kind: AuthChallengeKind): void {
    this.authChallengeKind.inc({ kind });
  }
  setRtpPortPoolInUse(v: number): void {
    this.rtpPortPoolInUse = v;
  }
  setRtpPortPoolSize(v: number): void {
    this.rtpPortPoolSize = v;
  }
  incRtpPortAcquireFailures(): void {
    this.rtpPortAcquireFailuresTotal++;
  }
  incStatusCallbackAttempts(event: StatusCallbackEvent): void {
    this.statusCallbackAttempts.inc({ event });
  }
  incStatusCallbackFailures(reason: StatusCallbackReason): void {
    this.statusCallbackFailures.inc({ reason });
  }

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

    // original 5
    emit('active_calls_total', 'gauge', 'Number of currently active calls', this.activeCalls);
    emit('sip_registration_status', 'gauge', '1 if registered with SIP registrar, 0 if not', this.sipRegistered);
    emit('rtp_packets_received_total', 'counter', 'Total RTP audio packets received from callers', this.rtpRxTotal);
    emit('rtp_packets_sent_total', 'counter', 'Total RTP audio packets sent to callers (incl. silence)', this.rtpTxTotal);
    emit('ws_reconnect_attempts_total', 'counter', 'Total WebSocket reconnect attempts across all calls', this.wsReconnectsTotal);

    // v3 labeled
    this.twimlParseErrors.emit(lines);
    this.apiRequests.emit(lines);
    this.apiRequestDuration.emit(lines);
    this.twimlModify.emit(lines);

    // v3 forwarding unlabeled
    emit('sipgate_bridge_forward_attempts_total', 'counter', 'Outbound <Dial> attempts that passed guardrails and entered the SIP forwarder.', this.forwardAttemptsTotal);
    emit('sipgate_bridge_forward_success_total', 'counter', 'Outbound dials that reached answered state.', this.forwardSuccessTotal);
    this.forwardFailed.emit(lines);
    this.forwardDuration.emit(lines);
    this.authChallengeKind.emit(lines);

    // v3 rtp port pool
    emit('sipgate_bridge_rtp_port_pool_in_use', 'gauge', 'Current count of allocated RTP ports.', this.rtpPortPoolInUse);
    emit('sipgate_bridge_rtp_port_pool_size', 'gauge', 'Total configured RTP ports (RTP_PORT_MAX - RTP_PORT_MIN + 1).', this.rtpPortPoolSize);
    emit('sipgate_bridge_rtp_port_acquire_failures_total', 'counter', 'Count of pool-exhausted AcquirePort calls.', this.rtpPortAcquireFailuresTotal);

    // v3 status callback
    this.statusCallbackAttempts.emit(lines);
    this.statusCallbackFailures.emit(lines);

    return lines.join('\n') + '\n';
  }
}
