<script>
  // Maps the 8 Twilio call statuses to a brand-token color family.
  // 'State'-style pill, mirroring @panda/ui's <State color="…"> look.
  let { status } = $props();

  const TONE = {
    'in-progress': 'success',
    completed: 'success',
    queued: 'accent',
    ringing: 'accent',
    failed: 'error',
    busy: 'error',
    'no-answer': 'error',
    canceled: 'error',
  };

  let tone = $derived(TONE[status] ?? 'neutral');
</script>

<span class="pill {tone}">{status}</span>

<style>
  .pill {
    display: inline-block;
    padding: 0.1rem 0.6rem;
    border-radius: 999px;
    font-size: 0.8rem;
    font-weight: 600;
    line-height: 1.5;
    white-space: nowrap;
  }
  .success {
    background: var(--neo-color-success-soft);
    color: var(--neo-color-success);
  }
  .accent {
    background: var(--neo-color-accent-soft);
    color: var(--neo-color-accent);
  }
  .error {
    background: var(--neo-color-error-soft);
    color: var(--neo-color-error);
  }
  .neutral {
    background: var(--neo-color-background-secondary);
    color: var(--neo-color-text-secondary);
  }
</style>
