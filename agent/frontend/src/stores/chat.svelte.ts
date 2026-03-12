export interface ChatMessage {
  id: string
  role: 'user' | 'agent' | 'tool' | 'error' | 'system'
  content: string
  toolName?: string
  toolStatus?: string
  timestamp: Date
}

interface AgentEvent {
  type: string
  content?: string
  tool_name?: string
}

// Reactive chat state using Svelte 5 $state rune at module level.
let messages = $state<ChatMessage[]>([])
let thinking = $state(false)
let connected = $state(false)
let model = $state('gemini-2.5-pro')

let nextId = 0
function genId(): string {
  return `msg-${++nextId}-${Date.now()}`
}

function addMessage(role: ChatMessage['role'], content: string, extra?: Partial<ChatMessage>) {
  messages.push({
    id: genId(),
    role,
    content,
    timestamp: new Date(),
    ...extra,
  })
}

// Initialize Wails event listeners.
function initEvents() {
  // @ts-ignore — Wails runtime injected globally
  if (typeof window.runtime === 'undefined') return

  // @ts-ignore
  window.runtime.EventsOn('agent:event', (evt: AgentEvent) => {
    switch (evt.type) {
      case 'tool_call':
        addMessage('tool', '', { toolName: evt.tool_name, toolStatus: 'calling' })
        break
      case 'tool_result': {
        // Update the last tool call message to show completion.
        const last = messages.findLast(m => m.role === 'tool' && m.toolName === evt.tool_name)
        if (last) last.toolStatus = 'done'
        break
      }
      case 'response':
        thinking = false
        addMessage('agent', evt.content ?? '')
        break
      case 'error':
        thinking = false
        addMessage('error', evt.content ?? 'Unknown error')
        break
      case 'done':
        thinking = false
        break
    }
  })

  // @ts-ignore
  window.runtime.EventsOn('agent:connected', () => {
    connected = true
  })

  // @ts-ignore
  window.runtime.EventsOn('agent:error', (msg: string) => {
    addMessage('error', msg)
  })

  // Load config from Go backend.
  // @ts-ignore
  if (window.go?.main?.App?.GetConfig) {
    // @ts-ignore
    window.go.main.App.GetConfig().then((cfg: { model: string }) => {
      model = cfg.model
    })
  }

  // Check initial connection.
  // @ts-ignore
  if (window.go?.main?.App?.CheckConnection) {
    // @ts-ignore
    window.go.main.App.CheckConnection().then((ok: boolean) => {
      connected = ok
    })
  }
}

// Slash command definitions.
interface Command {
  description: string
  usage?: string
  execute: (args: string[]) => Promise<void> | void
}

// Resolve a workflow ID prefix — if the arg is short, find the matching workflow.
async function resolveWorkflowID(idPrefix: string): Promise<string | null> {
  if (!idPrefix) return null
  // If it looks like a full UUID, use as-is.
  if (idPrefix.length >= 32) return idPrefix
  // Try to resolve from workflows list.
  // @ts-ignore
  if (!window.go?.main?.App?.ListWorkflows) return idPrefix
  try {
    // @ts-ignore
    const list = await window.go.main.App.ListWorkflows()
    if (!list) return idPrefix
    const matches = list.filter((w: { aggregate_id: string }) =>
      w.aggregate_id.startsWith(idPrefix)
    )
    if (matches.length === 1) return matches[0].aggregate_id
    if (matches.length > 1) {
      addMessage('system', `Ambiguous ID prefix \`${idPrefix}\` — matches ${matches.length} workflows. Use more characters.`)
      return null
    }
  } catch { /* fall through */ }
  return idPrefix
}

function requireBinding(method: string): boolean {
  // @ts-ignore
  if (!window.go?.main?.App?.[method]) {
    addMessage('system', 'Wails bindings not available.')
    return false
  }
  return true
}

function buildCommands(): Record<string, Command> {
  return {
    clear: {
      description: 'Clear chat history and operator context',
      execute: async () => {
        messages.length = 0
        nextId = 0
        // Reset the operator's conversation context.
        if (requireBinding('ClearContext')) {
          // @ts-ignore
          const err = await window.go.main.App.ClearContext()
          if (err) {
            addMessage('error', `Context reset failed: ${err}`)
            return
          }
        }
        addMessage('system', 'Chat and operator context cleared.')
      },
    },
    help: {
      description: 'Show available commands',
      execute: () => {
        const cmds = buildCommands()
        const lines = Object.entries(cmds)
          .map(([name, cmd]) => {
            const usage = cmd.usage ? ` ${cmd.usage}` : ''
            return `**/${name}${usage}** — ${cmd.description}`
          })
          .join('\n')
        addMessage('system', `Available commands:\n\n${lines}`)
      },
    },
    status: {
      description: 'Check rick-server connectivity',
      execute: async () => {
        if (!requireBinding('CheckConnection')) return
        // @ts-ignore
        const ok = await window.go.main.App.CheckConnection()
        connected = ok
        addMessage('system', ok ? 'rick-server: **connected**' : 'rick-server: **unreachable**')
      },
    },
    reconnect: {
      description: 'Re-discover tools from MCP server',
      execute: async () => {
        addMessage('system', 'Reconnecting...')
        if (!requireBinding('Reconnect')) return
        // @ts-ignore
        const err = await window.go.main.App.Reconnect()
        if (err) {
          addMessage('error', `Reconnect failed: ${err}`)
        } else {
          connected = true
          addMessage('system', 'Reconnected. Tools re-discovered.')
        }
      },
    },
    config: {
      description: 'Show current configuration',
      execute: async () => {
        if (!requireBinding('GetConfig')) return
        // @ts-ignore
        const cfg = await window.go.main.App.GetConfig()
        addMessage('system', `**Model:** ${cfg.model}\n**Server:** ${cfg.server_url || cfg.ServerURL || 'unknown'}`)
      },
    },
    workflows: {
      description: 'Quick-list all workflows',
      execute: async () => {
        if (!requireBinding('ListWorkflows')) return
        // @ts-ignore
        const list = await window.go.main.App.ListWorkflows()
        if (!list || list.length === 0) {
          addMessage('system', 'No workflows found.')
          return
        }
        const rows = list.map((w: { aggregate_id: string; workflow_id: string; status: string }) =>
          `| \`${w.aggregate_id.slice(0, 8)}\` | ${w.workflow_id} | ${w.status} |`
        )
        addMessage('system', `| ID | Workflow | Status |\n|---|---|---|\n${rows.join('\n')}`)
      },
    },
    deadletters: {
      description: 'Check dead letter queue',
      execute: async () => {
        if (!requireBinding('ListDeadLetters')) return
        // @ts-ignore
        const list = await window.go.main.App.ListDeadLetters()
        if (!list || list.length === 0) {
          addMessage('system', 'Dead letter queue is empty.')
          return
        }
        const rows = list.map((d: { handler: string; error: string; attempts: number }) =>
          `| ${d.handler} | ${d.error.slice(0, 60)} | ${d.attempts} |`
        )
        addMessage('system', `**${list.length} dead letter(s):**\n\n| Handler | Error | Attempts |\n|---|---|---|\n${rows.join('\n')}`)
      },
    },
    model: {
      description: 'Show current AI model',
      execute: () => {
        addMessage('system', `Current model: **${model}**`)
      },
    },

    // --- Memory ---
    remember: {
      description: 'Save a memory for future sessions',
      usage: '[category] <text>',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/remember [category] text`\nCategories: user, preference, environment, workflow, general'); return }
        if (!requireBinding('SaveMemory')) return
        // If the first arg looks like a category, use it; otherwise default to "general".
        const categories = ['user', 'preference', 'environment', 'workflow', 'general']
        let category = 'general'
        let text: string
        if (categories.includes(args[0].toLowerCase())) {
          category = args[0].toLowerCase()
          text = args.slice(1).join(' ')
        } else {
          text = args.join(' ')
        }
        if (!text) { addMessage('system', 'Usage: `/remember [category] text`'); return }
        // @ts-ignore
        const mem = await window.go.main.App.SaveMemory(text, category)
        if (mem) {
          addMessage('system', `Saved memory \`${mem.id}\` [${mem.category}]: ${mem.content}`)
        } else {
          addMessage('error', 'Failed to save memory.')
        }
      },
    },
    memories: {
      description: 'List all saved memories',
      execute: async () => {
        if (!requireBinding('ListMemories')) return
        // @ts-ignore
        const list = await window.go.main.App.ListMemories()
        if (!list || list.length === 0) {
          addMessage('system', 'No memories saved. Use `/remember [category] text` to save one.')
          return
        }
        const rows = list.map((m: { id: string; category: string; content: string }) =>
          `| \`${m.id}\` | ${m.category} | ${m.content} |`
        )
        addMessage('system', `**Saved memories (${list.length}):**\n\n| ID | Category | Content |\n|---|---|---|\n${rows.join('\n')}`)
      },
    },
    forget: {
      description: 'Delete a saved memory',
      usage: '<id>',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/forget <id>` — use `/memories` to see IDs'); return }
        if (!requireBinding('DeleteMemory')) return
        // @ts-ignore
        const ok = await window.go.main.App.DeleteMemory(args[0])
        if (ok) {
          addMessage('system', `Memory \`${args[0]}\` deleted.`)
        } else {
          addMessage('system', `Memory \`${args[0]}\` not found. Use \`/memories\` to list all.`)
        }
      },
    },

    // --- Workflow Control ---
    cancel: {
      description: 'Cancel a running workflow',
      usage: '<id> [reason]',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/cancel <id> [reason]`'); return }
        if (!requireBinding('CancelWorkflow')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        const reason = args.slice(1).join(' ')
        // @ts-ignore
        const result = await window.go.main.App.CancelWorkflow(id, reason)
        addMessage('system', `Workflow \`${id.slice(0, 8)}\` **cancelled**. ${reason ? `Reason: ${reason}` : ''}`)
      },
    },
    pause: {
      description: 'Pause a running workflow',
      usage: '<id> [reason]',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/pause <id> [reason]`'); return }
        if (!requireBinding('PauseWorkflow')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        const reason = args.slice(1).join(' ')
        // @ts-ignore
        const result = await window.go.main.App.PauseWorkflow(id, reason)
        addMessage('system', `Workflow \`${id.slice(0, 8)}\` **paused**. ${reason ? `Reason: ${reason}` : ''}`)
      },
    },
    resume: {
      description: 'Resume a paused workflow',
      usage: '<id> [reason]',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/resume <id> [reason]`'); return }
        if (!requireBinding('ResumeWorkflow')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        const reason = args.slice(1).join(' ')
        // @ts-ignore
        const result = await window.go.main.App.ResumeWorkflow(id, reason)
        addMessage('system', `Workflow \`${id.slice(0, 8)}\` **resumed**. ${reason ? `Reason: ${reason}` : ''}`)
      },
    },

    // --- Workflow Inspection ---
    events: {
      description: 'List recent events (global or per-workflow)',
      usage: '[id] [limit]',
      execute: async (args) => {
        if (!requireBinding('ListEvents')) return
        let workflowID = ''
        let limit = 20
        if (args[0]) {
          const id = await resolveWorkflowID(args[0])
          if (!id) return
          workflowID = id
        }
        if (args[1]) limit = parseInt(args[1], 10) || 20
        // @ts-ignore
        const list = await window.go.main.App.ListEvents(workflowID, limit)
        if (!list || list.length === 0) {
          addMessage('system', 'No events found.')
          return
        }
        const rows = list.map((e: { type: string; timestamp: string; source: string; correlation_id?: string }) => {
          const ts = e.timestamp?.slice(11, 19) || ''
          const corr = e.correlation_id ? `\`${e.correlation_id.slice(0, 8)}\`` : ''
          return `| ${ts} | ${e.type} | ${e.source || ''} | ${corr} |`
        })
        const header = workflowID ? `Events for \`${workflowID.slice(0, 8)}\`` : `Recent events (last ${limit})`
        addMessage('system', `**${header}:**\n\n| Time | Type | Source | Workflow |\n|---|---|---|---|\n${rows.join('\n')}`)
      },
    },
    tokens: {
      description: 'Show token usage for a workflow',
      usage: '<id>',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/tokens <id>`'); return }
        if (!requireBinding('TokenUsageForWorkflow')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        // @ts-ignore
        const usage = await window.go.main.App.TokenUsageForWorkflow(id)
        if (!usage) { addMessage('system', 'No token data found.'); return }
        let out = `**Token usage for \`${id.slice(0, 8)}\`:** ${usage.total.toLocaleString()} total\n\n`
        if (usage.by_phase && Object.keys(usage.by_phase).length > 0) {
          out += '**By phase:**\n'
          out += Object.entries(usage.by_phase)
            .sort(([, a], [, b]) => (b as number) - (a as number))
            .map(([phase, count]) => `- ${phase}: ${(count as number).toLocaleString()}`)
            .join('\n')
        }
        if (usage.by_backend && Object.keys(usage.by_backend).length > 0) {
          out += '\n\n**By backend:**\n'
          out += Object.entries(usage.by_backend)
            .map(([backend, count]) => `- ${backend}: ${(count as number).toLocaleString()}`)
            .join('\n')
        }
        addMessage('system', out)
      },
    },
    phases: {
      description: 'Show phase timeline for a workflow',
      usage: '<id>',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/phases <id>`'); return }
        if (!requireBinding('PhaseTimeline')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        // @ts-ignore
        const list = await window.go.main.App.PhaseTimeline(id)
        if (!list || list.length === 0) { addMessage('system', 'No phase data found.'); return }
        const rows = list.map((p: { phase: string; status: string; iterations: number; duration_ms?: number }) => {
          const dur = p.duration_ms ? `${(p.duration_ms / 1000).toFixed(1)}s` : '-'
          return `| ${p.phase} | ${p.status} | ${p.iterations} | ${dur} |`
        })
        addMessage('system', `**Phases for \`${id.slice(0, 8)}\`:**\n\n| Phase | Status | Iter | Duration |\n|---|---|---|---|\n${rows.join('\n')}`)
      },
    },
    verdicts: {
      description: 'Show review verdicts for a workflow',
      usage: '<id>',
      execute: async (args) => {
        if (!args[0]) { addMessage('system', 'Usage: `/verdicts <id>`'); return }
        if (!requireBinding('WorkflowVerdicts')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        // @ts-ignore
        const list = await window.go.main.App.WorkflowVerdicts(id)
        if (!list || list.length === 0) { addMessage('system', 'No verdicts found.'); return }
        const lines = list.map((v: { phase: string; source_phase: string; outcome: string; summary: string; issues?: { severity: string; description: string }[] }) => {
          const icon = v.outcome === 'pass' ? '✅' : '❌'
          let line = `${icon} **${v.phase}** (reviewing ${v.source_phase}): ${v.summary}`
          if (v.issues && v.issues.length > 0) {
            line += '\n' + v.issues.map(i => `  - [${i.severity}] ${i.description}`).join('\n')
          }
          return line
        })
        addMessage('system', `**Verdicts for \`${id.slice(0, 8)}\`:**\n\n${lines.join('\n\n')}`)
      },
    },

    // --- Hint Management ---
    approve: {
      description: 'Approve a pending hint',
      usage: '<id> <persona> [guidance]',
      execute: async (args) => {
        if (!args[0] || !args[1]) { addMessage('system', 'Usage: `/approve <id> <persona> [guidance]`'); return }
        if (!requireBinding('ApproveHint')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        const persona = args[1]
        const guidance = args.slice(2).join(' ')
        // @ts-ignore
        await window.go.main.App.ApproveHint(id, persona, guidance)
        addMessage('system', `Hint for **${persona}** in \`${id.slice(0, 8)}\` **approved**. ${guidance ? `Guidance: ${guidance}` : ''}`)
      },
    },
    reject: {
      description: 'Reject a pending hint (skip persona)',
      usage: '<id> <persona> [reason]',
      execute: async (args) => {
        if (!args[0] || !args[1]) { addMessage('system', 'Usage: `/reject <id> <persona> [reason]`'); return }
        if (!requireBinding('RejectHint')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        const persona = args[1]
        const reason = args.slice(2).join(' ')
        // @ts-ignore
        await window.go.main.App.RejectHint(id, persona, reason, 'skip')
        addMessage('system', `Hint for **${persona}** in \`${id.slice(0, 8)}\` **rejected** (skipped). ${reason ? `Reason: ${reason}` : ''}`)
      },
    },

    // --- Operator Intervention ---
    guide: {
      description: 'Inject operator guidance into a workflow',
      usage: '<id> <message>',
      execute: async (args) => {
        if (!args[0] || !args[1]) { addMessage('system', 'Usage: `/guide <id> <message>`'); return }
        if (!requireBinding('InjectGuidance')) return
        const id = await resolveWorkflowID(args[0])
        if (!id) return
        const content = args.slice(1).join(' ')
        // @ts-ignore
        await window.go.main.App.InjectGuidance(id, content, '')
        addMessage('system', `Guidance injected into \`${id.slice(0, 8)}\`: ${content}`)
      },
    },
  }
}

async function handleCommand(text: string): Promise<boolean> {
  if (!text.startsWith('/')) return false

  const parts = text.slice(1).split(/\s+/)
  const name = parts[0]?.toLowerCase()
  if (!name) return false

  const cmds = buildCommands()
  const cmd = cmds[name]
  if (!cmd) {
    addMessage('system', `Unknown command: \`/${name}\`. Type \`/help\` for available commands.`)
    return true
  }

  try {
    await cmd.execute(parts.slice(1))
  } catch (e) {
    addMessage('error', `Command /${name} failed: ${e instanceof Error ? e.message : String(e)}`)
  }
  return true
}

async function sendMessage(text: string) {
  // Intercept slash commands — never hits the LLM.
  if (text.startsWith('/')) {
    addMessage('user', text)
    await handleCommand(text)
    return
  }

  addMessage('user', text)
  thinking = true

  // @ts-ignore — Wails runtime
  if (window.go?.main?.App?.SendMessage) {
    // @ts-ignore
    window.go.main.App.SendMessage(text)
  }
}

// Command metadata for autocomplete — derived from buildCommands().
export interface CommandMeta {
  name: string
  description: string
  usage?: string
}

export function getCommandMeta(): CommandMeta[] {
  const cmds = buildCommands()
  return Object.entries(cmds).map(([name, cmd]) => ({
    name,
    description: cmd.description,
    usage: cmd.usage,
  }))
}

// Initialize on load.
if (typeof window !== 'undefined') {
  // Wails injects runtime after DOM ready, so defer.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initEvents)
  } else {
    initEvents()
  }
}

export const chatStore = {
  get messages() { return messages },
  get thinking() { return thinking },
  get connected() { return connected },
  get model() { return model },
  sendMessage,
}
