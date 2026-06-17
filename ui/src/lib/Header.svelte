<script>
  let { accountSid, onLogout } = $props();
</script>

<header>
  <div class="brand">
    <span class="logo" aria-hidden="true"></span>
    <span class="titles">
      <span class="mark">sipgate SIP Stream Bridge</span>
      <span class="sub">operator console</span>
    </span>
  </div>
  <div class="right">
    {#if accountSid}
      <code class="sid" title="Account SID">{accountSid}</code>
    {/if}
    <button type="button" onclick={onLogout}>Logout</button>
  </div>
</header>

<style>
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    padding: 1rem 1.5rem;
    background: var(--neo-color-ink);
    color: var(--neo-color-on-ink);
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }
  /* The lime square is the only lime on screen — a small brand accent. */
  .logo {
    width: 1.15rem;
    height: 1.15rem;
    border-radius: 2px;
    background: var(--neo-color-brand-primary);
    flex: none;
  }
  .titles {
    display: flex;
    flex-direction: column;
    line-height: 1.2;
  }
  .mark {
    font-weight: 700;
    font-size: 1.15rem;
    letter-spacing: -0.01em;
  }
  .sub {
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.14em;
    color: var(--neo-color-on-ink-muted);
  }
  .right {
    display: flex;
    align-items: center;
    gap: 0.9rem;
  }
  .sid {
    font-family: var(--neo-font-mono);
    font-size: 0.78rem;
    color: var(--neo-color-on-ink-muted);
  }
  button {
    background: transparent;
    color: var(--neo-color-on-ink);
    border: 1px solid var(--neo-color-on-ink-muted);
    border-radius: var(--neo-radius);
    padding: 0.3rem 0.7rem;
    font-size: 0.8rem;
    font-weight: 600;
    cursor: pointer;
  }
  button:hover {
    background: rgba(255, 255, 255, 0.1);
  }
</style>
