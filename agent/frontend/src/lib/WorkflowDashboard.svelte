<script lang="ts">
  import { dashboardStore } from '../stores/dashboard.svelte'
  import WorkflowCard from './WorkflowCard.svelte'
  import DeadLetterBanner from './DeadLetterBanner.svelte'

  let workflows = $derived(dashboardStore.workflows)
  let deadLetterCount = $derived(dashboardStore.deadLetterCount)
  let serverReachable = $derived(dashboardStore.serverReachable)
  let error = $derived(dashboardStore.error)

  $effect(() => {
    dashboardStore.startPolling('full')
    return () => dashboardStore.stopPolling()
  })
</script>

<div class="flex-1 overflow-y-auto dashboard-scroll px-6 py-4 space-y-3">
  {#if !serverReachable}
    <div class="px-4 py-2.5 rounded-lg border-l-2 border-red-400 bg-red-50 text-red-600 text-sm">
      Server unreachable — dashboard data may be stale.
      {#if error}<span class="text-red-500 ml-2">{error}</span>{/if}
    </div>
  {/if}

  {#if deadLetterCount > 0}
    <DeadLetterBanner count={deadLetterCount} />
  {/if}

  {#if workflows.length === 0}
    <div class="flex flex-col items-center justify-center h-full text-gray-400">
      <p class="text-base font-medium mb-1 text-gray-500">No workflows</p>
      <p class="text-sm text-center">Start a workflow from the Chat tab to see it here.</p>
    </div>
  {:else}
    {#each workflows as workflow (workflow.aggregate_id)}
      <WorkflowCard {workflow} />
    {/each}
  {/if}
</div>
