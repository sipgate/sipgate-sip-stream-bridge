<script>
  import StatusPill from './StatusPill.svelte';

  let { calls } = $props();

  function fmtDuration(d) {
    if (d === null || d === undefined || d === '') return '—';
    const n = Number(d);
    if (!Number.isFinite(n)) return '—';
    const m = Math.floor(n / 60);
    const s = n % 60;
    return `${m}:${String(s).padStart(2, '0')}`;
  }

  function fmtTime(t) {
    if (!t) return '—';
    const parsed = new Date(t);
    return Number.isNaN(parsed.getTime()) ? t : parsed.toLocaleTimeString();
  }
</script>

<section class="panel">
  <div class="head">
    <h2>Calls</h2>
    <span class="count">{calls.length}</span>
  </div>

  {#if calls.length === 0}
    <p class="empty">No active or recently-terminated calls.</p>
  {:else}
    <table>
      <thead>
        <tr>
          <th>Status</th>
          <th>Dir</th>
          <th>From</th>
          <th>To</th>
          <th>Started</th>
          <th>Duration</th>
          <th>Call SID</th>
        </tr>
      </thead>
      <tbody>
        {#each calls as call (call.sid)}
          <tr>
            <td><StatusPill status={call.status} /></td>
            <td class="dir">{call.direction}</td>
            <td>{call.from}</td>
            <td>{call.to}</td>
            <td>{fmtTime(call.start_time)}</td>
            <td>{fmtDuration(call.duration)}</td>
            <td><code>{call.sid}</code></td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</section>

<style>
  .panel {
    margin: var(--neo-gap);
    background: var(--neo-color-background-primary);
    border: 1px solid var(--neo-color-border-primary);
    /* thin lime top rule — the brand accent, mirroring sipgate's card style */
    border-top: 3px solid var(--neo-color-brand-primary);
    border-radius: var(--neo-radius);
    overflow: hidden;
  }
  .head {
    display: flex;
    align-items: center;
    gap: 0.6rem;
    padding: 0.9rem 1.2rem;
    border-bottom: 1px solid var(--neo-color-border-primary);
  }
  h2 {
    margin: 0;
    font-size: 1rem;
  }
  .count {
    background: var(--neo-color-ink);
    color: var(--neo-color-on-ink);
    border-radius: 999px;
    padding: 0.05rem 0.55rem;
    font-size: 0.8rem;
    font-weight: 700;
  }
  .empty {
    margin: 0;
    padding: 2rem 1.2rem;
    color: var(--neo-color-text-secondary);
    text-align: center;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.9rem;
  }
  th {
    text-align: left;
    padding: 0.6rem 1.2rem;
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--neo-color-text-secondary);
    background: var(--neo-color-background-secondary);
  }
  td {
    padding: 0.7rem 1.2rem;
    border-top: 1px solid var(--neo-color-border-primary);
    vertical-align: middle;
  }
  .dir {
    text-transform: capitalize;
    color: var(--neo-color-text-secondary);
  }
  code {
    font-family: var(--neo-font-mono);
    font-size: 0.78rem;
    color: var(--neo-color-text-secondary);
  }
</style>
