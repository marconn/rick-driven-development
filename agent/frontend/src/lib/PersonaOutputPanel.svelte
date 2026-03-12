<script lang="ts">
  import type { PersonaOutput } from '../stores/dashboard.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'
  import { renderMarkdown } from '../utils/markdown'

  let output = $derived(
    dashboardStore.outputModalPersona
      ? dashboardStore.personaOutputs[dashboardStore.outputModalPersona] ?? null
      : null
  )

  function close() {
    dashboardStore.closeOutputModal()
  }

  function formatDuration(ms: number): string {
    const s = Math.floor(ms / 1000)
    if (s < 60) return `${s}s`
    const m = Math.floor(s / 60)
    return `${m}m ${s % 60}s`
  }
</script>

{#if output}
  <div
    class="fixed inset-0 z-50 flex flex-col bg-white"
    role="dialog"
    aria-label="Persona output"
    tabindex="-1"
    onkeydown={(e) => { if (e.key === 'Escape') close() }}
  >
    <div class="shrink-0 border-b border-gray-200 bg-white px-6 py-4 flex items-center justify-between">
      <div class="flex items-center gap-4">
        <span class="text-lg font-mono font-semibold text-gray-800">{output.persona}</span>
        <span class="text-base text-purple-600">{output.backend}</span>
        {#if output.tokens_used > 0}
          <span class="text-base text-gray-400">{output.tokens_used.toLocaleString()} tokens</span>
        {/if}
        {#if output.duration_ms > 0}
          <span class="text-base text-gray-400">{formatDuration(output.duration_ms)}</span>
        {/if}
      </div>
      <button
        onclick={close}
        class="p-2 rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-100 transition-colors"
        title="Close (Esc)"
      >
        <svg class="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <line x1="18" y1="6" x2="6" y2="18"/>
          <line x1="6" y1="6" x2="18" y2="18"/>
        </svg>
      </button>
    </div>

    {#if output.truncated}
      <div class="shrink-0 bg-amber-50 border-b border-amber-200 px-6 py-2">
        <span class="text-sm text-amber-600">Truncated — showing first 10,000 characters</span>
      </div>
    {/if}

    <div class="flex-1 overflow-y-auto px-6 py-6 dashboard-scroll">
      <div class="max-w-3xl mx-auto markdown-content text-gray-700">
        {@html renderMarkdown(output.output || 'No output available.')}
      </div>
    </div>

    <div class="shrink-0 border-t border-gray-200 bg-white px-6 py-3 flex justify-end">
      <button
        onclick={close}
        class="px-4 py-2 text-base rounded-lg bg-gray-200 text-gray-700 hover:bg-gray-300 transition-colors"
      >
        Close
      </button>
    </div>
  </div>
{/if}
