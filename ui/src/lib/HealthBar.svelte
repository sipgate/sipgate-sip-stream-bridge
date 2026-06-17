<script>
  let { health, error, lastUpdated, serverDown = false } = $props();

  let registered = $derived(!serverDown && health?.registered === true);
  let stamp = $derived(
    lastUpdated ? new Date(lastUpdated).toLocaleTimeString() : '—',
  );
</script>

{#if serverDown}
  <p class="down" role="alert">
    <span class="dot bad"></span>
    Server unreachable — the bridge is probably offline. Last data at {stamp}.
  </p>
{/if}

<section class="bar" class:stale={serverDown}>
  <div class="stat">
    <span class="label">SIP registration</span>
    <span class="value">
      <span class="dot" class:ok={registered} class:bad={!registered}></span>
      {serverDown ? 'unknown' : registered ? 'registered' : 'not registered'}
    </span>
  </div>
  <div class="stat">
    <span class="label">Active calls</span>
    <span class="value">{serverDown ? '—' : (health?.active_calls ?? '—')}</span>
  </div>
  <div class="stat">
    <span class="label">Active forwards</span>
    <span class="value">{serverDown ? '—' : (health?.active_forwards ?? '—')}</span>
  </div>
  <div class="stat right">
    <span class="label">Last updated</span>
    <span class="value">{stamp}</span>
  </div>
</section>

{#if error && !serverDown}
  <p class="error">⚠ {error}</p>
{/if}

<style>
  .bar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--neo-gap);
    align-items: stretch;
    padding: 1rem 1.5rem;
    background: var(--neo-color-background-primary);
    border-bottom: 1px solid var(--neo-color-border-primary);
  }
  .stat {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
  }
  .stat.right {
    margin-left: auto;
    text-align: right;
  }
  .label {
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    color: var(--neo-color-text-secondary);
  }
  .value {
    font-size: 1.1rem;
    font-weight: 700;
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  .dot {
    width: 0.6rem;
    height: 0.6rem;
    border-radius: 50%;
    display: inline-block;
  }
  .dot.ok {
    background: var(--neo-color-success);
  }
  .dot.bad {
    background: var(--neo-color-error);
  }
  .error {
    margin: 0;
    padding: 0.6rem 1.5rem;
    background: var(--neo-color-error-soft);
    color: var(--neo-color-error);
    font-weight: 600;
  }
  .down {
    margin: 0;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.7rem 1.5rem;
    background: var(--neo-color-error);
    color: #fff;
    font-weight: 700;
  }
  .down .dot.bad {
    background: #fff;
  }
  .bar.stale {
    opacity: 0.55;
  }
</style>
