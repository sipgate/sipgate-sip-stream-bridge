<script>
  let { accountSid = '', error = '', onLogin } = $props();
  let password = $state('');
  let busy = $state(false);

  async function submit(e) {
    e.preventDefault();
    if (busy || password.length === 0) return;
    busy = true;
    try {
      await onLogin(password);
    } finally {
      busy = false;
    }
  }
</script>

<div class="wrap">
  <form onsubmit={submit}>
    <div class="brand">
      <span class="logo" aria-hidden="true"></span>
      <div>
        <div class="mark">sipgate SIP Stream Bridge</div>
        <div class="sub">operator console</div>
      </div>
    </div>

    <label>
      <span>Username</span>
      <input type="text" value="admin" disabled />
    </label>
    <label>
      <span>Password (AUTH_TOKEN)</span>
      <!-- svelte-ignore a11y_autofocus -->
      <input type="password" bind:value={password} autocomplete="current-password" autofocus />
    </label>

    {#if error}<p class="err">{error}</p>{/if}

    <button type="submit" disabled={busy || password.length === 0}>
      {busy ? 'Signing in…' : 'Sign in'}
    </button>

    {#if accountSid}
      <code class="sid" title="Account SID (used as the API username)">{accountSid}</code>
    {/if}
  </form>
</div>

<style>
  .wrap {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 1.5rem;
  }
  form {
    width: 100%;
    max-width: 360px;
    display: flex;
    flex-direction: column;
    gap: 1rem;
    padding: 2rem;
    background: var(--neo-color-background-primary);
    border: 1px solid var(--neo-color-border-primary);
    border-top: 3px solid var(--neo-color-brand-primary);
    border-radius: var(--neo-radius);
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    margin-bottom: 0.5rem;
  }
  .logo {
    width: 1.15rem;
    height: 1.15rem;
    border-radius: 2px;
    background: var(--neo-color-brand-primary);
    flex: none;
  }
  .mark {
    font-weight: 700;
    font-size: 1.05rem;
    letter-spacing: -0.01em;
  }
  .sub {
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.14em;
    color: var(--neo-color-text-secondary);
  }
  label {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
    font-size: 0.8rem;
    color: var(--neo-color-text-secondary);
  }
  input {
    padding: 0.55rem 0.7rem;
    border: 1px solid var(--neo-color-border-primary);
    border-radius: var(--neo-radius);
    font-size: 0.95rem;
    font-family: var(--neo-font);
  }
  input:disabled {
    background: var(--neo-color-background-secondary);
    color: var(--neo-color-text-secondary);
  }
  button {
    margin-top: 0.25rem;
    padding: 0.65rem 1rem;
    border: none;
    border-radius: var(--neo-radius);
    background: var(--neo-color-ink);
    color: var(--neo-color-on-ink);
    font-weight: 700;
    font-size: 0.95rem;
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .err {
    margin: 0;
    color: var(--neo-color-error);
    font-size: 0.85rem;
    font-weight: 600;
  }
  .sid {
    font-family: var(--neo-font-mono);
    font-size: 0.72rem;
    color: var(--neo-color-text-secondary);
    word-break: break-all;
  }
</style>
