import { describe, it, expect } from 'vitest';
import {
  Metrics,
  bucketStatus,
  bucketOutcome,
  bucketForwardReason,
  bucketStatusCallbackReason,
} from '../src/observability/metrics.js';

describe('Metrics — original 5 still emit', () => {
  it('emits the original gauges/counters', () => {
    const m = new Metrics();
    m.incActiveCalls();
    m.setSipRegistered(true);
    m.incRtpRx();
    m.incRtpTx();
    m.incWsReconnect();

    const out = m.getPrometheus();
    expect(out).toContain('active_calls_total 1');
    expect(out).toContain('sip_registration_status 1');
    expect(out).toContain('rtp_packets_received_total 1');
    expect(out).toContain('rtp_packets_sent_total 1');
    expect(out).toContain('ws_reconnect_attempts_total 1');

    expect(m.getHealth()).toEqual({ registered: true, activeCalls: 1 });
  });
});

describe('Metrics — v3 labeled counters render correct Prometheus lines', () => {
  it('twiml_parse_errors_total{code}', () => {
    const m = new Metrics();
    m.incTwimlParseError('12100');
    m.incTwimlParseError('12100');
    m.incTwimlParseError('21218');
    const out = m.getPrometheus();
    expect(out).toContain('twiml_parse_errors_total{code="12100"} 2');
    expect(out).toContain('twiml_parse_errors_total{code="21218"} 1');
  });

  it('api_requests_total{route,method,status}', () => {
    const m = new Metrics();
    m.incApiRequest('list_calls', 'GET', '2xx');
    m.incApiRequest('list_calls', 'GET', '2xx');
    m.incApiRequest('modify_call', 'POST', '4xx');
    const out = m.getPrometheus();
    expect(out).toContain('api_requests_total{route="list_calls",method="GET",status="2xx"} 2');
    expect(out).toContain('api_requests_total{route="modify_call",method="POST",status="4xx"} 1');
  });

  it('twiml_modify_total{kind,outcome}', () => {
    const m = new Metrics();
    m.incTwimlModify('twiml', 'ok');
    m.incTwimlModify('url', 'fetch_error');
    const out = m.getPrometheus();
    expect(out).toContain('twiml_modify_total{kind="twiml",outcome="ok"} 1');
    expect(out).toContain('twiml_modify_total{kind="url",outcome="fetch_error"} 1');
  });

  it('forward attempt/success/failed + auth challenge', () => {
    const m = new Metrics();
    m.incForwardAttempts();
    m.incForwardSuccess();
    m.incForwardFailed('busy');
    m.incForwardFailed('busy');
    m.incAuthChallengeKind('407');
    const out = m.getPrometheus();
    expect(out).toContain('sipgate_bridge_forward_attempts_total 1');
    expect(out).toContain('sipgate_bridge_forward_success_total 1');
    expect(out).toContain('sipgate_bridge_forward_failed_total{reason="busy"} 2');
    expect(out).toContain('sipgate_bridge_auth_challenge_kind_total{kind="407"} 1');
  });

  it('rtp port pool gauges + acquire failures', () => {
    const m = new Metrics();
    m.setRtpPortPoolSize(100);
    m.setRtpPortPoolInUse(4);
    m.incRtpPortAcquireFailures();
    const out = m.getPrometheus();
    expect(out).toContain('sipgate_bridge_rtp_port_pool_size 100');
    expect(out).toContain('sipgate_bridge_rtp_port_pool_in_use 4');
    expect(out).toContain('sipgate_bridge_rtp_port_acquire_failures_total 1');
  });

  it('status callback attempts + failures', () => {
    const m = new Metrics();
    m.incStatusCallbackAttempts('completed');
    m.incStatusCallbackFailures('timeout');
    const out = m.getPrometheus();
    expect(out).toContain('sipgate_bridge_status_callback_attempts_total{event="completed"} 1');
    expect(out).toContain('sipgate_bridge_status_callback_failures_total{reason="timeout"} 1');
  });
});

describe('Metrics — histograms emit _bucket/_sum/_count', () => {
  it('api_request_duration_seconds{route}', () => {
    const m = new Metrics();
    m.observeApiRequestDuration('get_call', 0.03);
    const out = m.getPrometheus();
    expect(out).toContain('# TYPE api_request_duration_seconds histogram');
    expect(out).toContain('api_request_duration_seconds_bucket{route="get_call",le="0.05"} 1');
    expect(out).toContain('api_request_duration_seconds_bucket{route="get_call",le="0.01"} 0');
    expect(out).toContain('api_request_duration_seconds_bucket{route="get_call",le="+Inf"} 1');
    expect(out).toContain('api_request_duration_seconds_count{route="get_call"} 1');
    expect(out).toContain('api_request_duration_seconds_sum{route="get_call"} 0.03');
  });

  it('forward_duration_seconds{outcome}', () => {
    const m = new Metrics();
    m.observeForwardDuration('answered', 3);
    const out = m.getPrometheus();
    expect(out).toContain('sipgate_bridge_forward_duration_seconds_bucket{outcome="answered",le="5"} 1');
    expect(out).toContain('sipgate_bridge_forward_duration_seconds_bucket{outcome="answered",le="2"} 0');
    expect(out).toContain('sipgate_bridge_forward_duration_seconds_count{outcome="answered"} 1');
  });
});

describe('bucketStatus', () => {
  it('maps codes to 2xx/4xx/5xx buckets', () => {
    expect(bucketStatus(0)).toBe('2xx');
    expect(bucketStatus(100)).toBe('2xx');
    expect(bucketStatus(200)).toBe('2xx');
    expect(bucketStatus(302)).toBe('2xx');
    expect(bucketStatus(400)).toBe('4xx');
    expect(bucketStatus(404)).toBe('4xx');
    expect(bucketStatus(499)).toBe('4xx');
    expect(bucketStatus(500)).toBe('5xx');
    expect(bucketStatus(503)).toBe('5xx');
    expect(bucketStatus(600)).toBe('5xx');
  });
});

describe('bucketOutcome', () => {
  it('maps dial status to bounded outcome enum', () => {
    expect(bucketOutcome('answered')).toBe('answered');
    expect(bucketOutcome('no-answer')).toBe('no_answer');
    expect(bucketOutcome('busy')).toBe('busy');
    expect(bucketOutcome('failed')).toBe('error');
    expect(bucketOutcome('')).toBe('error');
    expect(bucketOutcome('completed')).toBe('error');
  });
});

describe('bucketForwardReason', () => {
  it('returns "" for nil/empty (success path)', () => {
    expect(bucketForwardReason(null)).toBe('');
    expect(bucketForwardReason(undefined)).toBe('');
    expect(bucketForwardReason('')).toBe('');
  });
  it('classifies known reasons (specific before generic)', () => {
    expect(bucketForwardReason(new Error('ErrTollFraudBlocked'))).toBe('toll_fraud');
    expect(bucketForwardReason('rate limit hit')).toBe('rate_limit');
    expect(bucketForwardReason('13214 caller-id rejected')).toBe('caller_id_rejected');
    expect(bucketForwardReason('486 Busy Here')).toBe('busy');
    expect(bucketForwardReason('603 Decline')).toBe('rejected');
    expect(bucketForwardReason('480 no-answer')).toBe('no_answer');
    expect(bucketForwardReason('codec mismatch')).toBe('codec_mismatch');
    expect(bucketForwardReason('407 auth required')).toBe('auth_failed');
    expect(bucketForwardReason('trunk 503 error')).toBe('trunk_5xx');
    expect(bucketForwardReason('context deadline')).toBe('timeout');
    expect(bucketForwardReason('host unreachable')).toBe('unreachable');
    expect(bucketForwardReason('something weird')).toBe('');
  });
});

describe('bucketStatusCallbackReason', () => {
  it('returns "" on success (no error + 2xx)', () => {
    expect(bucketStatusCallbackReason(null, 200)).toBe('');
    expect(bucketStatusCallbackReason(null, 204)).toBe('');
  });
  it('classifies typed sentinels and status ranges', () => {
    expect(bucketStatusCallbackReason(new Error('per-call status callback queue full'), 0)).toBe('queue_full');
    expect(bucketStatusCallbackReason(new Error('status callback retries exhausted'), 0)).toBe('exhausted_retries');
    expect(bucketStatusCallbackReason(new Error('callback URL targets blocked address space'), 0)).toBe('ssrf_rejected');
    expect(bucketStatusCallbackReason(new Error('request timeout'), 0)).toBe('timeout');
    expect(bucketStatusCallbackReason(new Error('dial tcp: connection refused'), 0)).toBe('connect_error');
    expect(bucketStatusCallbackReason(null, 503)).toBe('5xx');
    expect(bucketStatusCallbackReason(null, 404)).toBe('4xx');
    expect(bucketStatusCallbackReason(new Error('weird'), 0)).toBe('connect_error');
  });
});
