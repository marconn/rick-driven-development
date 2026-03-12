<script lang="ts">
  import type { WorkflowDetail, PhaseEntry, TokenUsage, VerdictRecord } from '../stores/dashboard.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'
  import VerdictPanel from './VerdictPanel.svelte'

  let { detail, phases, tokens, verdicts }: {
    detail: WorkflowDetail
    phases: PhaseEntry[]
    tokens: TokenUsage | null
    verdicts: VerdictRecord[]
  } = $props()

  function handlePersonaClick(persona: string) {
    if (detail.completed_personas[persona]) {
      dashboardStore.fetchPersonaOutput(detail.id, persona)
    }
  }

  let maxPhaseTokens = $derived(
    tokens ? Math.max(...Object.values(tokens.by_phase), 1) : 1
  )
  let maxBackendTokens = $derived(
    tokens ? Math.max(...Object.values(tokens.by_backend), 1) : 1
  )

  let noOpPhases = $derived(new Set(
    phases.filter(p => p.duration_ms === 0 && p.status !== 'running').map(p => p.phase)
  ))

  let sortedPersonas = $derived.by(() => {
    const entries = Object.entries(detail.completed_personas ?? {}).filter(([name]) => !noOpPhases.has(name))
    const phaseOrder = new Map(phases.map((p, i) => [p.phase, i]))
    return entries.sort((a, b) => {
      const ia = phaseOrder.get(a[0]) ?? 999
      const ib = phaseOrder.get(b[0]) ?? 999
      if (ia !== ib) return ia - ib
      return a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0
    })
  })

  let sortedFeedback = $derived(
    Object.entries(detail.feedback_count ?? {}).sort((a, b) =>
      a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0
    )
  )

  let sortedPhaseTokens = $derived(
    Object.entries(tokens?.by_phase ?? {}).sort((a, b) => b[1] - a[1])
  )

  let sortedBackendTokens = $derived(
    Object.entries(tokens?.by_backend ?? {}).sort((a, b) => b[1] - a[1])
  )
</script>

<div class="border-t border-gray-100 px-4 py-3 space-y-4">
  {#if tokens && tokens.total > 0}
    <div>
      <h4 class="text-base font-medium text-gray-500 mb-2">Token Usage ({tokens.total.toLocaleString()} total)</h4>
      <div class="grid grid-cols-2 gap-4">
        <div class="space-y-1">
          <span class="text-sm text-gray-400">By Phase</span>
          {#each sortedPhaseTokens as [phase, count]}
            <div class="flex items-center gap-2">
              <span class="text-sm text-gray-500 w-20 truncate">{phase}</span>
              <div class="proportion-bar flex-1">
                <div class="proportion-fill bg-blue-400" style="width: {(count / maxPhaseTokens) * 100}%"></div>
              </div>
              <span class="text-sm text-gray-400 w-12 text-right">{count.toLocaleString()}</span>
            </div>
          {/each}
        </div>
        <div class="space-y-1">
          <span class="text-sm text-gray-400">By Backend</span>
          {#each sortedBackendTokens as [backend, count]}
            <div class="flex items-center gap-2">
              <span class="text-sm text-gray-500 w-20 truncate">{backend}</span>
              <div class="proportion-bar flex-1">
                <div class="proportion-fill bg-purple-400" style="width: {(count / maxBackendTokens) * 100}%"></div>
              </div>
              <span class="text-sm text-gray-400 w-12 text-right">{count.toLocaleString()}</span>
            </div>
          {/each}
        </div>
      </div>
    </div>
  {/if}

  {#if detail.completed_personas && Object.keys(detail.completed_personas).length > 0}
    <div>
      <h4 class="text-base font-medium text-gray-500 mb-2">Personas</h4>
      <div class="flex flex-wrap gap-2">
        {#each sortedPersonas as [persona, done]}
          {#if done}
            <button
              onclick={() => handlePersonaClick(persona)}
              class="px-2 py-0.5 text-sm rounded-full bg-emerald-50 text-emerald-700 hover:bg-emerald-100 cursor-pointer transition-colors"
              title="Click to view AI output"
            >
              {persona}
            </button>
          {:else}
            <span class="px-2 py-0.5 text-sm rounded-full bg-gray-100 text-gray-400">
              {persona}
            </span>
          {/if}
        {/each}
      </div>
    </div>
  {/if}

  {#if detail.feedback_count && Object.keys(detail.feedback_count).length > 0}
    <div>
      <h4 class="text-base font-medium text-gray-500 mb-2">Feedback Iterations</h4>
      <div class="flex flex-wrap gap-2">
        {#each sortedFeedback as [phase, count]}
          <span class="px-2 py-0.5 text-sm rounded-full bg-amber-50 text-amber-700">
            {phase}: {count} iteration{count !== 1 ? 's' : ''}
          </span>
        {/each}
      </div>
    </div>
  {/if}

  {#if verdicts && verdicts.length > 0}
    <VerdictPanel {verdicts} />
  {/if}
</div>
