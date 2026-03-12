<script lang="ts">
  import { chatStore } from '../stores/chat.svelte'

  let connected = $derived(chatStore.connected)
  let thinking = $derived(chatStore.thinking)
  let model = $derived(chatStore.model)
</script>

<div class="flex items-center justify-between px-5 py-2 bg-white border-b border-gray-200 select-none"
     style="--wails-draggable: drag">
  <div class="flex items-center gap-2">
    <span class="text-sm font-semibold tracking-wide text-gray-400 uppercase">Rick</span>
  </div>

  <div class="flex items-center gap-4 text-sm">
    {#if thinking}
      <div class="flex items-center gap-1.5 text-gray-500">
        <svg class="w-3.5 h-3.5 animate-spin" viewBox="0 0 24 24" fill="none">
          <circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="3" opacity="0.25"/>
          <path d="M4 12a8 8 0 018-8" stroke="currentColor" stroke-width="3" stroke-linecap="round"/>
        </svg>
        <span>Thinking</span>
      </div>
    {/if}

    <span class="text-gray-300">{model}</span>

    <div class="flex items-center gap-1.5">
      <div class="w-1.5 h-1.5 rounded-full {connected ? 'bg-emerald-500' : 'bg-red-400'}"></div>
      <span class="text-gray-400">{connected ? 'Connected' : 'Offline'}</span>
    </div>
  </div>
</div>
