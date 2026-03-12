<script lang="ts">
  import { eventsStore } from '../stores/events.svelte'
  import EventFilterBar from './EventFilterBar.svelte'
  import EventList from './EventList.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'

  let serverReachable = $derived(dashboardStore.serverReachable)

  $effect(() => {
    eventsStore.startPolling()
    return () => eventsStore.stopPolling()
  })
</script>

<div class="flex-1 flex flex-col overflow-hidden">
  {#if !serverReachable}
    <div class="px-6 py-2 bg-red-50 border-b border-red-200 text-red-600 text-base">
      Server unreachable — event stream paused.
    </div>
  {/if}

  <EventFilterBar />
  <EventList />
</div>
