<script lang="ts">
  import type { PhaseEntry } from '../stores/dashboard.svelte'

  let { phases, hintedPersonas = [] }: { phases: PhaseEntry[]; hintedPersonas?: string[] } = $props()

  let hintedSet = $derived(new Set(hintedPersonas))

  let visiblePhases = $derived(
    phases
      .filter(p => p.duration_ms !== 0 || p.status === 'running' || hintedSet.has(p.phase))
      .sort((a, b) => {
        if (!a.started_at && !b.started_at) return 0
        if (!a.started_at) return 1
        if (!b.started_at) return -1
        return a.started_at < b.started_at ? -1 : a.started_at > b.started_at ? 1 : 0
      })
  )

  function barColor(status: string, phase: string): string {
    if (hintedSet.has(phase)) return 'bg-teal-400 phase-bar-hint'
    switch (status) {
      case 'running': return 'bg-blue-400 phase-bar-running'
      case 'completed': return 'bg-emerald-400'
      case 'failed': return 'bg-red-400'
      default: return 'bg-gray-200 border border-dashed border-gray-300'
    }
  }

  let maxDuration = $derived(
    Math.max(...visiblePhases.map(p => p.duration_ms ?? 0), 1)
  )
</script>

<div class="space-y-1.5">
  {#each visiblePhases as phase}
    <div class="flex items-center gap-2">
      <span class="text-base text-gray-500 w-44 truncate text-right font-mono">{phase.phase}</span>
      <div class="flex-1 bg-gray-100 rounded h-1.5 overflow-hidden">
        {#if phase.duration_ms}
          <div
            class="phase-bar {barColor(phase.status, phase.phase)}"
            style="width: {Math.max((phase.duration_ms / maxDuration) * 100, 2)}%; height: 100%;"
          ></div>
        {:else if hintedSet.has(phase.phase)}
          <div class="phase-bar bg-teal-400 phase-bar-hint" style="width: 40%; height: 100%;"></div>
        {:else if phase.status === 'running'}
          <div class="phase-bar bg-blue-400 phase-bar-running" style="width: 40%; height: 100%;"></div>
        {:else}
          <div class="phase-bar bg-gray-200" style="width: 0%; height: 100%;"></div>
        {/if}
      </div>
      <span class="text-sm w-14 text-right {hintedSet.has(phase.phase) ? 'text-teal-600' : 'text-gray-400'}">
        {#if hintedSet.has(phase.phase)}
          hint
        {:else if phase.duration_ms}
          {phase.duration_ms >= 1000 ? `${(phase.duration_ms / 1000).toFixed(1)}s` : `${phase.duration_ms}ms`}
        {:else if phase.status === 'running'}
          ...
        {:else}
          --
        {/if}
      </span>
    </div>
  {/each}
</div>
