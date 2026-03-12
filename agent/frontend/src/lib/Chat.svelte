<script lang="ts">
  import { chatStore } from '../stores/chat.svelte'
  import Message from './Message.svelte'
  import { tick } from 'svelte'

  let chatContainer: HTMLDivElement

  let messages = $derived(chatStore.messages)
  let thinking = $derived(chatStore.thinking)

  $effect(() => {
    if (messages.length > 0) {
      tick().then(() => {
        if (chatContainer) {
          chatContainer.scrollTop = chatContainer.scrollHeight
        }
      })
    }
  })
</script>

<div
  bind:this={chatContainer}
  class="flex-1 overflow-y-auto chat-scroll px-6 py-6"
>
  {#if messages.length === 0}
    <div class="flex flex-col items-center justify-center h-full">
      <p class="text-lg font-medium mb-2 text-gray-700">Rick Operator</p>
      <p class="text-base text-center max-w-md leading-relaxed text-gray-400">
        Ask about workflows, check status, start tasks, or inject guidance.
      </p>
      <div class="mt-5 flex flex-wrap justify-center gap-2 text-base">
        <span class="px-3 py-1.5 rounded-md bg-gray-50 border border-gray-200 text-gray-500 cursor-default">"List all workflows"</span>
        <span class="px-3 py-1.5 rounded-md bg-gray-50 border border-gray-200 text-gray-500 cursor-default">"What's running?"</span>
        <span class="px-3 py-1.5 rounded-md bg-gray-50 border border-gray-200 text-gray-500 cursor-default">"Start a workflow"</span>
        <span class="px-3 py-1.5 rounded-md bg-gray-50 border border-gray-200 text-gray-500 cursor-default">"Show dead letters"</span>
      </div>
      <div class="mt-3 text-base text-gray-400">
        Type <code class="px-1.5 py-0.5 rounded bg-gray-100 text-gray-500 font-mono">/help</code> for commands
      </div>
    </div>
  {/if}

  <div class="max-w-3xl mx-auto space-y-4">
    {#each messages as msg (msg.id)}
      <Message message={msg} />
    {/each}
  </div>

  {#if thinking}
    <div class="max-w-3xl mx-auto flex items-center gap-1.5 px-4 py-3 text-gray-400">
      <span class="typing-dot w-1.5 h-1.5 rounded-full bg-gray-400"></span>
      <span class="typing-dot w-1.5 h-1.5 rounded-full bg-gray-400"></span>
      <span class="typing-dot w-1.5 h-1.5 rounded-full bg-gray-400"></span>
    </div>
  {/if}
</div>
