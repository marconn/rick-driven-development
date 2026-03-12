<script lang="ts">
  import type { WorkflowSummary } from '../stores/dashboard.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'

  let { workflow }: { workflow: WorkflowSummary } = $props()

  let showGuidance = $state(false)
  let guidanceContent = $state('')
  let guidanceTarget = $state('')
  let acting = $state(false)

  async function handlePause() {
    acting = true
    await dashboardStore.pauseWorkflow(workflow.aggregate_id, 'operator requested')
    acting = false
  }

  async function handleCancel() {
    acting = true
    await dashboardStore.cancelWorkflow(workflow.aggregate_id, 'operator requested')
    acting = false
  }

  async function handleResume() {
    acting = true
    await dashboardStore.resumeWorkflow(workflow.aggregate_id, 'operator requested')
    acting = false
  }

  async function handleGuidance() {
    if (!guidanceContent.trim()) return
    acting = true
    await dashboardStore.injectGuidance(workflow.aggregate_id, guidanceContent, guidanceTarget)
    guidanceContent = ''
    guidanceTarget = ''
    showGuidance = false
    acting = false
  }
</script>

<div class="flex items-center gap-1" style="--wails-draggable: no-drag">
  {#if workflow.status === 'running'}
    <button
      onclick={handlePause}
      disabled={acting}
      title="Pause workflow"
      class="p-1 rounded text-gray-400 hover:text-amber-500 hover:bg-gray-100 transition-colors disabled:opacity-50"
    >
      <svg class="w-4 h-4" viewBox="0 0 24 24" fill="currentColor">
        <rect x="6" y="4" width="4" height="16" rx="1"/>
        <rect x="14" y="4" width="4" height="16" rx="1"/>
      </svg>
    </button>

    <button
      onclick={handleCancel}
      disabled={acting}
      title="Cancel workflow"
      class="p-1 rounded text-gray-400 hover:text-red-500 hover:bg-gray-100 transition-colors disabled:opacity-50"
    >
      <svg class="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <line x1="18" y1="6" x2="6" y2="18"/>
        <line x1="6" y1="6" x2="18" y2="18"/>
      </svg>
    </button>
  {/if}

  {#if workflow.status === 'paused'}
    <button
      onclick={handleResume}
      disabled={acting}
      title="Resume workflow"
      class="p-1 rounded text-gray-400 hover:text-emerald-500 hover:bg-gray-100 transition-colors disabled:opacity-50"
    >
      <svg class="w-4 h-4" viewBox="0 0 24 24" fill="currentColor">
        <polygon points="5,3 19,12 5,21"/>
      </svg>
    </button>
  {/if}

  {#if workflow.status === 'running' || workflow.status === 'paused'}
    <button
      onclick={() => showGuidance = !showGuidance}
      disabled={acting}
      title="Inject guidance"
      class="p-1 rounded text-gray-400 hover:text-blue-500 hover:bg-gray-100 transition-colors disabled:opacity-50"
    >
      <svg class="w-4 h-4" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <path d="M21 15a2 2 0 01-2 2H7l-4 4V5a2 2 0 012-2h14a2 2 0 012 2z"/>
      </svg>
    </button>
  {/if}
</div>

{#if showGuidance}
  <div class="fixed inset-0 bg-black/20 flex items-center justify-center z-50"
       role="presentation" onclick={() => showGuidance = false} onkeydown={(e) => { if (e.key === 'Escape') showGuidance = false }}>
    <div class="bg-white border border-gray-200 rounded-lg p-5 w-96 max-w-[90vw] shadow-lg"
         role="dialog" aria-label="Inject guidance" tabindex="-1" onclick={(e) => e.stopPropagation()} onkeydown={() => {}}>
      <h3 class="text-base font-semibold text-gray-800 mb-3">Inject Guidance</h3>
      <textarea
        bind:value={guidanceContent}
        placeholder="Guidance for the next persona..."
        rows="3"
        class="w-full resize-none bg-gray-50 border border-gray-200 rounded-lg px-3 py-2 text-base
               text-gray-800 placeholder-gray-400 focus:outline-none focus:border-gray-400 mb-2"
      ></textarea>
      <input
        type="text"
        bind:value={guidanceTarget}
        placeholder="Target persona (optional)"
        class="w-full bg-gray-50 border border-gray-200 rounded-lg px-3 py-2 text-base
               text-gray-800 placeholder-gray-400 focus:outline-none focus:border-gray-400 mb-3"
      />
      <div class="flex justify-end gap-2">
        <button
          onclick={() => showGuidance = false}
          class="px-3 py-1.5 text-base rounded-lg text-gray-500 hover:text-gray-700 hover:bg-gray-100"
        >
          Cancel
        </button>
        <button
          onclick={handleGuidance}
          disabled={acting || !guidanceContent.trim()}
          class="px-3 py-1.5 text-base rounded-lg bg-gray-800 text-white hover:bg-gray-700
                 disabled:opacity-50 disabled:cursor-not-allowed"
        >
          Send
        </button>
      </div>
    </div>
  </div>
{/if}
