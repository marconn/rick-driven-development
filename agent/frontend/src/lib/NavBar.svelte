<script lang="ts">
  import { chatStore } from '../stores/chat.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'

  type Tab = 'chat' | 'workflows' | 'events'
  let { activeTab, onTabChange }: { activeTab: Tab, onTabChange: (tab: Tab) => void } = $props()

  let connected = $derived(chatStore.connected)
  let thinking = $derived(chatStore.thinking)
  let model = $derived(chatStore.model)
  let activeCount = $derived(dashboardStore.activeCount)
  let failedCount = $derived(dashboardStore.failedCount)
  let pausedCount = $derived(dashboardStore.pausedCount)

  let healthText = $derived.by(() => {
    const parts: string[] = []
    if (activeCount > 0) parts.push(`${activeCount} running`)
    if (failedCount > 0) parts.push(`${failedCount} failed`)
    if (pausedCount > 0) parts.push(`${pausedCount} paused`)
    return parts.length > 0 ? parts.join(' · ') : 'idle'
  })

  let healthColor = $derived.by(() => {
    if (failedCount > 0) return 'text-red-500'
    if (activeCount > 0) return 'text-emerald-600'
    if (pausedCount > 0) return 'text-amber-500'
    return 'text-gray-400'
  })

  const tabs: { id: Tab, label: string }[] = [
    { id: 'chat', label: 'Chat' },
    { id: 'workflows', label: 'Workflows' },
    { id: 'events', label: 'Events' },
  ]
</script>

<div class="flex items-center justify-between px-5 h-12 bg-white border-b border-gray-200 select-none"
     style="--wails-draggable: drag">
  <div class="flex items-center gap-6">
    <span class="text-base font-semibold tracking-wide text-gray-400 uppercase">Rick</span>

    <nav class="flex items-center gap-1" style="--wails-draggable: no-drag">
      {#each tabs as tab}
        <button
          onclick={() => onTabChange(tab.id)}
          class="px-3 py-1.5 text-base font-medium rounded-md transition-colors
            {activeTab === tab.id
              ? 'text-gray-800 bg-gray-100'
              : 'text-gray-400 hover:text-gray-600 hover:bg-gray-50'}"
        >
          {tab.label}
        </button>
      {/each}
    </nav>
  </div>

  <div class="flex items-center gap-4 text-base">
    {#if thinking}
      <div class="flex items-center gap-1.5 text-gray-500">
        <svg class="w-3.5 h-3.5 animate-spin" viewBox="0 0 24 24" fill="none">
          <circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="3" opacity="0.25"/>
          <path d="M4 12a8 8 0 018-8" stroke="currentColor" stroke-width="3" stroke-linecap="round"/>
        </svg>
        <span>Thinking</span>
      </div>
    {/if}

    <span class="{healthColor}">{healthText}</span>
    <span class="text-gray-300">{model}</span>

    <div class="flex items-center gap-1.5">
      <div class="w-2 h-2 rounded-full {connected ? 'bg-emerald-500' : 'bg-red-400'}"></div>
      <span class="text-gray-400">{connected ? 'Connected' : 'Offline'}</span>
    </div>
  </div>
</div>
