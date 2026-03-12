<script lang="ts">
  import type { PendingHint } from '../stores/dashboard.svelte'
  import { dashboardStore } from '../stores/dashboard.svelte'
  import { renderMarkdown } from '../utils/markdown'

  let { hints, workflowId }: { hints: PendingHint[]; workflowId: string } = $props()

  let acting = $state<string | null>(null)
  let reviewingHint = $state<PendingHint | null>(null)
  let showGuidance = $state(false)
  let guidance = $state('')
  let rejectReason = $state('')
  let showReject = $state(false)

  function openReview(hint: PendingHint) {
    reviewingHint = hint
    showGuidance = false
    showReject = false
    guidance = ''
    rejectReason = ''
  }

  function closeReview() {
    reviewingHint = null
    showGuidance = false
    showReject = false
    guidance = ''
    rejectReason = ''
  }

  async function handleApprove(persona: string) {
    acting = persona
    const g = showGuidance ? guidance : ''
    await dashboardStore.approveHint(workflowId, persona, g)
    acting = null
    closeReview()
  }

  async function handleReject(persona: string, action: 'skip' | 'fail') {
    acting = persona
    await dashboardStore.rejectHint(workflowId, persona, rejectReason, action)
    acting = null
    closeReview()
  }

  function confidenceColor(c: number): string {
    if (c >= 0.7) return 'text-emerald-600'
    if (c >= 0.4) return 'text-amber-600'
    return 'text-red-500'
  }

  function confidenceBg(c: number): string {
    if (c >= 0.7) return 'bg-emerald-400'
    if (c >= 0.4) return 'bg-amber-400'
    return 'bg-red-400'
  }
</script>

<div class="border border-teal-200 bg-teal-50/50 rounded-lg p-3 space-y-3">
  <div class="flex items-center gap-2">
    <svg class="w-5 h-5 text-teal-600 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
      <circle cx="12" cy="12" r="10"/>
      <line x1="12" y1="8" x2="12" y2="12"/>
      <line x1="12" y1="16" x2="12.01" y2="16"/>
    </svg>
    <span class="text-base font-medium text-teal-700">Pending — Review Required</span>
  </div>

  {#each hints as hint}
    <div class="border border-gray-200 bg-white rounded-lg p-3">
      <div class="flex items-center justify-between">
        <div class="flex items-center gap-3">
          <span class="text-base font-mono font-semibold text-gray-800">{hint.persona}</span>
          <div class="flex items-center gap-2">
            <div class="w-20 h-2 bg-gray-200 rounded-full overflow-hidden">
              <div class="h-full rounded-full {confidenceBg(hint.confidence)}" style="width: {hint.confidence * 100}%"></div>
            </div>
            <span class="text-base {confidenceColor(hint.confidence)}">
              {(hint.confidence * 100).toFixed(0)}%
            </span>
          </div>
          {#if hint.blockers && hint.blockers.length > 0}
            <span class="text-base text-red-500">{hint.blockers.length} blocker{hint.blockers.length > 1 ? 's' : ''}</span>
          {/if}
          {#if hint.token_estimate > 0}
            <span class="text-sm text-gray-400">~{hint.token_estimate.toLocaleString()} tokens</span>
          {/if}
        </div>

        <button
          onclick={() => openReview(hint)}
          class="px-4 py-2 text-base font-medium rounded-lg bg-teal-600 text-white hover:bg-teal-500 transition-colors"
        >
          Review Plan
        </button>
      </div>
    </div>
  {/each}
</div>

{#if reviewingHint}
  <div
    class="fixed inset-0 z-50 flex flex-col bg-white"
    role="dialog"
    aria-label="Review hint plan"
    tabindex="-1"
    onkeydown={(e) => { if (e.key === 'Escape') closeReview() }}
  >
    <div class="shrink-0 border-b border-gray-200 bg-white px-6 py-4 flex items-center justify-between">
      <div class="flex items-center gap-4">
        <span class="text-lg font-mono font-semibold text-gray-800">{reviewingHint.persona}</span>
        <div class="flex items-center gap-2">
          <div class="w-24 h-2.5 bg-gray-200 rounded-full overflow-hidden">
            <div class="h-full rounded-full {confidenceBg(reviewingHint.confidence)}" style="width: {reviewingHint.confidence * 100}%"></div>
          </div>
          <span class="text-base font-medium {confidenceColor(reviewingHint.confidence)}">
            {(reviewingHint.confidence * 100).toFixed(0)}% confidence
          </span>
        </div>
        {#if reviewingHint.token_estimate > 0}
          <span class="text-base text-gray-400">~{reviewingHint.token_estimate.toLocaleString()} tokens estimated</span>
        {/if}
      </div>
      <button
        onclick={closeReview}
        class="p-2 rounded-lg text-gray-400 hover:text-gray-600 hover:bg-gray-100 transition-colors"
        title="Close (Esc)"
      >
        <svg class="w-6 h-6" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <line x1="18" y1="6" x2="6" y2="18"/>
          <line x1="6" y1="6" x2="18" y2="18"/>
        </svg>
      </button>
    </div>

    {#if reviewingHint.blockers && reviewingHint.blockers.length > 0}
      <div class="shrink-0 bg-red-50 border-b border-red-200 px-6 py-3">
        <div class="flex items-start gap-2">
          <span class="text-base font-medium text-red-600 shrink-0">Blockers:</span>
          <div class="flex flex-wrap gap-2">
            {#each reviewingHint.blockers as blocker}
              <span class="px-2 py-0.5 text-base rounded bg-red-100 text-red-700">{blocker}</span>
            {/each}
          </div>
        </div>
      </div>
    {/if}

    <div class="flex-1 overflow-y-auto px-6 py-6 dashboard-scroll">
      <div class="max-w-3xl mx-auto markdown-content text-gray-700">
        {@html renderMarkdown(reviewingHint.plan || 'No plan content.')}
      </div>
    </div>

    <div class="shrink-0 border-t border-gray-200 bg-white px-6 py-4">
      {#if showReject}
        <div class="flex items-center gap-3 max-w-3xl mx-auto">
          <input
            type="text"
            bind:value={rejectReason}
            placeholder="Reason for rejection (optional)..."
            class="flex-1 bg-gray-50 border border-gray-200 rounded-lg px-4 py-2.5 text-base
                   text-gray-800 placeholder-gray-400 focus:outline-none focus:border-gray-400"
          />
          <button
            onclick={() => handleReject(reviewingHint.persona, 'skip')}
            disabled={acting === reviewingHint.persona}
            class="px-4 py-2.5 text-base rounded-lg bg-gray-200 text-gray-700 hover:bg-gray-300
                   disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap"
          >
            Skip (mark complete)
          </button>
          <button
            onclick={() => handleReject(reviewingHint.persona, 'fail')}
            disabled={acting === reviewingHint.persona}
            class="px-4 py-2.5 text-base rounded-lg bg-red-500 text-white hover:bg-red-400
                   disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap"
          >
            Fail workflow
          </button>
          <button
            onclick={() => { showReject = false; rejectReason = '' }}
            class="px-4 py-2.5 text-base rounded-lg text-gray-500 hover:text-gray-700 hover:bg-gray-100"
          >
            Back
          </button>
        </div>
      {:else}
        <div class="flex items-center gap-3 max-w-3xl mx-auto">
          {#if showGuidance}
            <textarea
              bind:value={guidance}
              placeholder="Guidance to adjust persona behavior before execution..."
              rows="2"
              class="flex-1 resize-none bg-gray-50 border border-gray-200 rounded-lg px-4 py-2.5 text-base
                     text-gray-800 placeholder-gray-400 focus:outline-none focus:border-gray-400"
            ></textarea>
          {:else}
            <div class="flex-1"></div>
          {/if}

          <button
            onclick={() => handleApprove(reviewingHint.persona)}
            disabled={acting === reviewingHint.persona}
            class="px-6 py-2.5 text-base font-medium rounded-lg bg-teal-600 text-white hover:bg-teal-500
                   disabled:opacity-50 disabled:cursor-not-allowed whitespace-nowrap"
          >
            {acting === reviewingHint.persona ? 'Approving...' : 'Approve & Execute'}
          </button>
          <button
            onclick={() => showGuidance = !showGuidance}
            disabled={acting === reviewingHint.persona}
            class="px-4 py-2.5 text-base rounded-lg text-gray-500 hover:text-teal-600 hover:bg-gray-100
                   disabled:opacity-50 whitespace-nowrap {showGuidance ? 'text-teal-600 bg-gray-100' : ''}"
          >
            {showGuidance ? 'Hide Guidance' : '+ Guidance'}
          </button>
          <button
            onclick={() => showReject = true}
            disabled={acting === reviewingHint.persona}
            class="px-4 py-2.5 text-base rounded-lg text-gray-400 hover:text-red-500 hover:bg-gray-100
                   disabled:opacity-50 whitespace-nowrap"
          >
            Reject
          </button>
        </div>
      {/if}
    </div>
  </div>
{/if}
