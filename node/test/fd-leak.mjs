/**
 * FD leak test: 20 sequential RTP allocations.
 *
 * Verifies that createRtpHandler + dispose() releases all file descriptors.
 * Satisfies WSR-03: after 20 sequential calls the process FD count returns
 * to the same baseline as after 0 calls (tolerance ±2).
 *
 * Run: node --import tsx/esm test/fd-leak.mjs
 */

import fs from 'node:fs';
import { execSync } from 'node:child_process';
import { createRtpHandler } from '../src/rtp/rtpHandler.js';

// ─── Cross-platform FD counter ────────────────────────────────────────────────

function getFdCount() {
  try {
    // Linux: subtract 1 to exclude the readdir FD itself (opens its own fd)
    return fs.readdirSync('/proc/self/fd').length - 1;
  } catch {
    // macOS / BSD fallback via lsof
    const out = execSync(`lsof -p ${process.pid} 2>/dev/null`).toString().trim();
    const lines = out.split('\n');
    return Math.max(0, lines.length - 1); // subtract 1 for header row
  }
}

// ─── No-op logger (avoids pino JSON noise during test) ───────────────────────

const noopLog = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
  trace: () => {},
};

// ─── Main ─────────────────────────────────────────────────────────────────────

async function main() {
  console.log('FD leak test: 20 sequential RTP allocations');

  // Use a port range distinct from production (10000–10099) to avoid conflicts
  const portMin = 20000;
  const portMax = 20099;

  const fdBefore = getFdCount();

  for (let i = 0; i < 20; i++) {
    const rtp = await createRtpHandler({ portMin, portMax, log: noopLog });
    rtp.dispose();
  }

  // Allow GC / OS to reclaim async close completions
  await new Promise((r) => setTimeout(r, 50));

  const fdAfter = getFdCount();
  const delta = fdAfter - fdBefore;

  console.log(`  Baseline FDs: ${fdBefore}`);
  console.log(`  Final FDs:    ${fdAfter}`);
  console.log(`  Delta:        ${delta}`);

  if (delta > 2) {
    console.log(`FAIL: FD leak detected — delta=${delta} after 20 calls (expected <= 2)`);
    process.exit(1);
  }

  console.log('PASS: no FD leak detected');
  process.exit(0);
}

main().catch((err) => {
  console.error('FATAL:', err.message ?? err);
  process.exit(1);
});
