<script>
  import { onMount } from 'svelte';
  import { getHealth, listCalls } from './lib/api.js';
  import Header from './lib/Header.svelte';
  import HealthBar from './lib/HealthBar.svelte';
  import CallList from './lib/CallList.svelte';
  import LoginForm from './lib/LoginForm.svelte';

  const POLL_MS = 3000;
  const TOKEN_KEY = 'sipbridge.token';

  let accountSid = $state('');
  let token = $state(sessionStorage.getItem(TOKEN_KEY) ?? '');
  let authed = $state(false);
  let health = $state(null);
  let calls = $state([]);
  let error = $state('');
  let loginError = $state('');
  let lastUpdated = $state(0);
  let serverDown = $state(false);
  let timer = null;

  // /health is public — gives us the AccountSid (the API username) before login.
  // Its reachability defines whether the server is up: a failed /health (network
  // error or non-2xx) means the bridge is almost certainly down.
  async function loadHealth() {
    health = await getHealth();
    if (health.account_sid) accountSid = health.account_sid;
  }

  async function poll() {
    // 1. Health probe — reachability == server up/down.
    try {
      await loadHealth();
      serverDown = false;
    } catch {
      serverDown = true;
      return; // server unreachable — skip the calls fetch this tick
    }
    // 2. Authenticated calls fetch.
    try {
      const page = await listCalls(accountSid, token);
      calls = page.calls ?? [];
      error = '';
      lastUpdated = Date.now();
    } catch (e) {
      if (e?.status === 401) {
        logout();
        return;
      }
      error = e instanceof Error ? e.message : String(e);
    }
  }

  function startPolling() {
    poll();
    timer = setInterval(poll, POLL_MS);
  }

  function logout() {
    authed = false;
    token = '';
    sessionStorage.removeItem(TOKEN_KEY);
    if (timer) {
      clearInterval(timer);
      timer = null;
    }
  }

  async function login(password) {
    loginError = '';
    try {
      if (!accountSid) await loadHealth();
      // Validate by calling the API with AccountSid:password.
      const page = await listCalls(accountSid, password);
      serverDown = false;
      token = password;
      sessionStorage.setItem(TOKEN_KEY, password);
      calls = page.calls ?? [];
      lastUpdated = Date.now();
      authed = true;
      startPolling();
    } catch (e) {
      if (e?.status === 401) {
        loginError = 'Incorrect password.';
      } else if (e?.status === undefined || e.status >= 500) {
        // No HTTP status (network failure) or 5xx → bridge unreachable.
        serverDown = true;
        loginError = 'Server unreachable — probably offline.';
      } else {
        loginError = e instanceof Error ? e.message : String(e);
      }
    }
  }

  onMount(() => {
    (async () => {
      try {
        await loadHealth();
        serverDown = false;
      } catch {
        serverDown = true; // bridge unreachable at load — polling will clear it on recovery
      }
      // Resume a stored session if the token still works.
      if (token) {
        try {
          const page = await listCalls(accountSid, token);
          calls = page.calls ?? [];
          lastUpdated = Date.now();
          authed = true;
          startPolling();
        } catch {
          logout();
        }
      }
    })();
    return () => {
      if (timer) clearInterval(timer);
    };
  });
</script>

{#if authed}
  <Header {accountSid} onLogout={logout} />
  <HealthBar {health} {error} {lastUpdated} {serverDown} />
  <CallList {calls} />
{:else}
  <LoginForm {accountSid} error={loginError} {serverDown} onLogin={login} />
{/if}
