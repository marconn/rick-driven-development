<script lang="ts">
  import { eventsStore } from '../stores/events.svelte'
  import EventRow from './EventRow.svelte'
  import { tick } from 'svelte'

  let events = $derived(eventsStore.events)
  let autoScroll = $derived(eventsStore.autoScroll)
  let listContainer: HTMLDivElement
  let showJumpButton = $state(false)

  function handleScroll() {
    if (!listContainer) return
    const { scrollTop, scrollHeight, clientHeight } = listContainer
    const atBottom = scrollHeight - scrollTop - clientHeight < 50
    if (!atBottom && autoScroll) {
      eventsStore.setAutoScroll(false)
    }
    showJumpButton = !atBottom
  }

  function jumpToLatest() {
    if (listContainer) {
      listContainer.scrollTop = listContainer.scrollHeight
      eventsStore.setAutoScroll(true)
      showJumpButton = false
    }
  }

  $effect(() => {
    if (autoScroll && events.length > 0) {
      tick().then(() => {
        if (listContainer) {
          listContainer.scrollTop = listContainer.scrollHeight
        }
      })
    }
  })
</script>

<div class="flex-1 relative overflow-hidden">
  <div
    bind:this={listContainer}
    onscroll={handleScroll}
    class="h-full overflow-y-auto dashboard-scroll"
  >
    {#if events.length === 0}
      <div class="flex items-center justify-center h-full text-gray-400 text-base">
        No events to show
      </div>
    {:else}
      {#each events as event (event.id)}
        <EventRow {event} />
      {/each}
    {/if}
  </div>

  {#if showJumpButton}
    <button
      onclick={jumpToLatest}
      class="absolute bottom-4 right-4 px-3 py-1.5 rounded-full bg-gray-800 text-white text-base
             shadow-lg hover:bg-gray-700 transition-colors"
    >
      Jump to latest
    </button>
  {/if}
</div>
