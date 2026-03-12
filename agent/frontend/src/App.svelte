<script lang="ts">
  import NavBar from './lib/NavBar.svelte'
  import Chat from './lib/Chat.svelte'
  import Input from './lib/Input.svelte'
  import WorkflowDashboard from './lib/WorkflowDashboard.svelte'
  import EventStream from './lib/EventStream.svelte'
  import { chatStore } from './stores/chat.svelte'
  import { dashboardStore } from './stores/dashboard.svelte'

  type Tab = 'chat' | 'workflows' | 'events'
  let activeTab = $state<Tab>('chat')

  function handleSend(text: string) {
    void chatStore.sendMessage(text)
  }

  function handleTabChange(tab: Tab) {
    activeTab = tab
  }

  // Keyboard shortcuts: Ctrl+1/2/3 for tab switching
  function handleKeydown(e: KeyboardEvent) {
    if (e.ctrlKey || e.metaKey) {
      if (e.key === '1') { activeTab = 'chat'; e.preventDefault() }
      if (e.key === '2') { activeTab = 'workflows'; e.preventDefault() }
      if (e.key === '3') { activeTab = 'events'; e.preventDefault() }
    }
  }

  // Start minimal polling for NavBar health pill on mount
  $effect(() => {
    dashboardStore.startPolling('minimal')
    return () => dashboardStore.stopPolling()
  })
</script>

<svelte:window onkeydown={handleKeydown} />

<div class="flex flex-col h-screen bg-white">
  <NavBar {activeTab} onTabChange={handleTabChange} />

  {#if activeTab === 'chat'}
    <Chat />
    <Input onSend={handleSend} />
  {:else if activeTab === 'workflows'}
    <WorkflowDashboard />
  {:else if activeTab === 'events'}
    <EventStream />
  {/if}
</div>
