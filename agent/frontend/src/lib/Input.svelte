<script lang="ts">
  import { chatStore, getCommandMeta, type CommandMeta } from '../stores/chat.svelte'

  let { onSend }: { onSend: (text: string) => void } = $props()
  let value = $state('')
  let textarea: HTMLTextAreaElement
  let thinking = $derived(chatStore.thinking)

  let showAutocomplete = $state(false)
  let selectedIndex = $state(0)
  let commandList: CommandMeta[] = []
  let filtered = $state<CommandMeta[]>([])

  function ensureCommands() {
    if (commandList.length === 0) {
      commandList = getCommandMeta()
    }
  }

  function updateAutocomplete() {
    if (!value.startsWith('/') || value.includes('\n')) {
      showAutocomplete = false
      return
    }

    ensureCommands()
    const query = value.slice(1).split(/\s/)[0].toLowerCase()

    if (value.includes(' ') && value.indexOf(' ') > 0) {
      showAutocomplete = false
      return
    }

    filtered = query
      ? commandList.filter(c => c.name.includes(query))
      : commandList

    showAutocomplete = filtered.length > 0
    selectedIndex = 0
  }

  function selectCommand(cmd: CommandMeta) {
    value = `/${cmd.name}${cmd.usage ? ' ' : ''}`
    showAutocomplete = false
    textarea?.focus()
    autoResize()
  }

  function handleKeydown(e: KeyboardEvent) {
    if (showAutocomplete) {
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        selectedIndex = (selectedIndex + 1) % filtered.length
        scrollSelectedIntoView()
        return
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault()
        selectedIndex = (selectedIndex - 1 + filtered.length) % filtered.length
        scrollSelectedIntoView()
        return
      }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
        e.preventDefault()
        if (filtered[selectedIndex]) {
          selectCommand(filtered[selectedIndex])
        }
        return
      }
      if (e.key === 'Escape') {
        e.preventDefault()
        showAutocomplete = false
        return
      }
    }

    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      submit()
    }
  }

  function scrollSelectedIntoView() {
    requestAnimationFrame(() => {
      const el = document.querySelector('.cmd-autocomplete-item.selected')
      el?.scrollIntoView({ block: 'nearest' })
    })
  }

  function submit() {
    const text = value.trim()
    if (!text || thinking) return
    showAutocomplete = false
    onSend(text)
    value = ''
    if (textarea) {
      textarea.style.height = 'auto'
    }
  }

  function autoResize() {
    if (textarea) {
      textarea.style.height = 'auto'
      textarea.style.height = Math.min(textarea.scrollHeight, 150) + 'px'
    }
  }

  function handleInput() {
    autoResize()
    updateAutocomplete()
  }
</script>

<div class="border-t border-gray-200 bg-white px-6 py-3 relative">
  {#if showAutocomplete}
    <div class="cmd-autocomplete">
      {#each filtered as cmd, i}
        <button
          class="cmd-autocomplete-item {i === selectedIndex ? 'selected' : ''}"
          onmousedown={(e) => { e.preventDefault(); selectCommand(cmd) }}
          onmouseenter={() => selectedIndex = i}
        >
          <span class="cmd-name">/{cmd.name}</span>
          {#if cmd.usage}
            <span class="cmd-usage">{cmd.usage}</span>
          {/if}
          <span class="cmd-desc">{cmd.description}</span>
        </button>
      {/each}
    </div>
  {/if}

  <div class="flex items-end gap-3 max-w-3xl mx-auto">
    <textarea
      bind:this={textarea}
      bind:value
      oninput={handleInput}
      onkeydown={handleKeydown}
      onblur={() => { setTimeout(() => showAutocomplete = false, 150) }}
      disabled={thinking}
      placeholder="Message Rick... or type / for commands"
      rows="1"
      class="flex-1 resize-none bg-gray-50 border border-gray-200 rounded-xl px-4 py-2.5 text-lg
             text-gray-800 placeholder-gray-400 focus:outline-none focus:border-gray-400
             disabled:opacity-50 disabled:cursor-not-allowed transition-colors"
    ></textarea>
    <button
      onclick={submit}
      disabled={thinking || !value.trim()}
      title="Send message"
      class="p-2.5 rounded-xl bg-gray-800 text-white hover:bg-gray-700
             disabled:opacity-30 disabled:cursor-not-allowed transition-colors"
    >
      <svg class="w-5 h-5" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
        <path d="M22 2L11 13M22 2l-7 20-4-9-9-4 20-7z" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
    </button>
  </div>
</div>
