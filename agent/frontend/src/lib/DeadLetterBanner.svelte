<script lang="ts">
  let { count }: { count: number } = $props()
</script>

<div class="px-4 py-2 rounded-lg border-l-2 border-red-400 bg-red-50 text-red-600 text-base flex items-center gap-2">
  <svg class="w-4 h-4 text-red-400 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
    <path d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z"/>
    <line x1="12" y1="9" x2="12" y2="13"/>
    <line x1="12" y1="17" x2="12.01" y2="17"/>
  </svg>
  <span>{count} dead letter{count !== 1 ? 's' : ''} detected — events failed delivery.</span>
</div>
