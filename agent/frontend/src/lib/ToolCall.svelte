<script lang="ts">
  let { toolName, status = 'calling' }: { toolName: string; status?: string } = $props()
</script>

<div class="flex justify-start px-1">
  <div class="flex items-center gap-2 px-3 py-1 rounded-full bg-gray-50 border border-gray-200 text-base text-gray-500">
    {#if status === 'calling'}
      <svg class="w-3.5 h-3.5 animate-spin text-gray-400" viewBox="0 0 24 24" fill="none">
        <circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="3" opacity="0.25"/>
        <path d="M4 12a8 8 0 018-8" stroke="currentColor" stroke-width="3" stroke-linecap="round"/>
      </svg>
    {:else}
      <svg class="w-3.5 h-3.5 text-emerald-500" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <path d="M20 6L9 17l-5-5" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
    {/if}
    <span class="font-mono">{toolName}</span>
  </div>
</div>
