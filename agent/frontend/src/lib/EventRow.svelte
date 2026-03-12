<script lang="ts">
  import type { EventEntry } from '../stores/events.svelte'
  import { eventColorClass } from '../stores/events.svelte'

  let { event }: { event: EventEntry } = $props()

  let expanded = $state(false)
  let colorClass = $derived(eventColorClass(event.type))

  let timeStr = $derived.by(() => {
    try {
      const d = new Date(event.timestamp)
      return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' })
    } catch {
      return event.timestamp
    }
  })
</script>

<div class="event-row border-b border-gray-100">
  <button
    onclick={() => expanded = !expanded}
    class="w-full flex items-center gap-3 px-6 py-1.5 text-left hover:bg-gray-50 transition-colors"
  >
    <span class="text-sm font-mono text-gray-400 w-16 shrink-0">{timeStr}</span>
    <span class="text-base font-mono {colorClass} truncate flex-1">{event.type}</span>
    <span class="text-sm text-gray-400 truncate max-w-32">{event.source}</span>
    <svg
      class="w-3 h-3 text-gray-400 shrink-0 transition-transform {expanded ? 'rotate-180' : ''}"
      viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"
    >
      <polyline points="6 9 12 15 18 9"/>
    </svg>
  </button>

  {#if expanded}
    <div class="px-6 py-2 bg-gray-50 border-t border-gray-100">
      <div class="grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
        <div><span class="text-gray-400">ID:</span> <span class="text-gray-600 font-mono">{event.id}</span></div>
        <div><span class="text-gray-400">Version:</span> <span class="text-gray-600">{event.version}</span></div>
        <div><span class="text-gray-400">Source:</span> <span class="text-gray-600">{event.source}</span></div>
        <div><span class="text-gray-400">Time:</span> <span class="text-gray-600">{event.timestamp}</span></div>
      </div>
    </div>
  {/if}
</div>
