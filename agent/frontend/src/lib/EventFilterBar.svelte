<script lang="ts">
  import { eventsStore } from '../stores/events.svelte'
  import type { EventCategory } from '../stores/events.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'

  let filters = $derived(eventsStore.filters)
  let autoScroll = $derived(eventsStore.autoScroll)
  let workflows = $derived(dashboardStore.workflows)

  const categories: { id: EventCategory, label: string }[] = [
    { id: 'all', label: 'All' },
    { id: 'lifecycle', label: 'Lifecycle' },
    { id: 'persona', label: 'Persona' },
    { id: 'ai', label: 'AI' },
    { id: 'feedback', label: 'Feedback' },
    { id: 'operator', label: 'Operator' },
    { id: 'hints', label: 'Hints' },
    { id: 'context', label: 'Context' },
    { id: 'sentinel', label: 'Sentinel' },
  ]
</script>

<div class="flex items-center gap-3 px-6 py-2 border-b border-gray-200 bg-gray-50/50">
  <select
    value={filters.category}
    onchange={(e) => eventsStore.setFilter('category', (e.target as HTMLSelectElement).value)}
    class="bg-white border border-gray-200 rounded-md px-2 py-1 text-base text-gray-700 focus:outline-none focus:border-gray-400"
  >
    {#each categories as cat}
      <option value={cat.id}>{cat.label}</option>
    {/each}
  </select>

  <select
    value={filters.correlationId}
    onchange={(e) => eventsStore.setFilter('correlationId', (e.target as HTMLSelectElement).value)}
    class="bg-white border border-gray-200 rounded-md px-2 py-1 text-base text-gray-700 focus:outline-none focus:border-gray-400"
  >
    <option value="">All Workflows</option>
    {#each workflows as wf}
      <option value={wf.aggregate_id}>{wf.aggregate_id.substring(0, 8)} — {wf.workflow_id}</option>
    {/each}
  </select>

  <input
    type="text"
    value={filters.search}
    oninput={(e) => eventsStore.setFilter('search', (e.target as HTMLInputElement).value)}
    placeholder="Search..."
    class="bg-white border border-gray-200 rounded-md px-2 py-1 text-base text-gray-700
           placeholder-gray-400 focus:outline-none focus:border-gray-400 flex-1 max-w-xs"
  />

  <label class="flex items-center gap-1.5 text-base text-gray-500 cursor-pointer ml-auto">
    <input
      type="checkbox"
      checked={autoScroll}
      onchange={(e) => eventsStore.setAutoScroll((e.target as HTMLInputElement).checked)}
      class="rounded border-gray-300 accent-gray-700"
    />
    Auto-scroll
  </label>
</div>
