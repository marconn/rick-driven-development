<script lang="ts">
  import { tick } from 'svelte'
  import type { ChatMessage } from '../stores/chat.svelte'
  import ToolCall from './ToolCall.svelte'
  import { renderMarkdown, renderMermaidIn } from '../utils/markdown'

  let { message }: { message: ChatMessage } = $props()

  let contentEl: HTMLElement | undefined = $state()

  $effect(() => {
    message.content
    if (contentEl) {
      tick().then(() => renderMermaidIn(contentEl!))
    }
  })
</script>

{#if message.role === 'user'}
  <div class="flex justify-end">
    <div class="max-w-[75%] px-4 py-2.5 rounded-lg bg-gray-100 text-gray-800 text-lg leading-relaxed">
      {message.content}
    </div>
  </div>
{:else if message.role === 'tool'}
  <ToolCall toolName={message.toolName ?? ''} status={message.toolStatus ?? 'calling'} />
{:else if message.role === 'error'}
  <div class="flex justify-start">
    <div class="max-w-[85%] px-4 py-2.5 rounded-lg border-l-2 border-red-400 bg-red-50 text-red-700 text-lg leading-relaxed">
      {message.content}
    </div>
  </div>
{:else if message.role === 'system'}
  <div class="flex justify-center">
    <div bind:this={contentEl} class="max-w-[85%] px-4 py-2 rounded-lg bg-gray-50 border border-gray-200 text-gray-500 text-base markdown-content">
      {@html renderMarkdown(message.content)}
    </div>
  </div>
{:else}
  <div class="flex justify-start">
    <div bind:this={contentEl} class="max-w-[85%] px-4 py-3 text-gray-700 text-lg markdown-content leading-relaxed">
      {@html renderMarkdown(message.content)}
    </div>
  </div>
{/if}
