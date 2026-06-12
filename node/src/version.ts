/**
 * Single source of truth for the SIP User-Agent / Server header.
 *
 * Emitted on every SIP request and response (REGISTER, OPTIONS, INVITE
 * responses, BYE, outbound INVITE). Reflects the product milestone this
 * implementation targets (v3 control plane).
 */
export const USER_AGENT = 'sipgate-sip-stream-bridge/3.0';
