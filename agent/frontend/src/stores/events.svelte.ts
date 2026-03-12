// Event stream store — chronological event feed with filtering.

export interface EventEntry {
  id: string
  type: string
  version: number
  timestamp: string
  source: string
  correlation_id?: string
  aggregate_id?: string
}

export type EventCategory = 'all' | 'lifecycle' | 'persona' | 'ai' | 'feedback' | 'operator' | 'hints' | 'context' | 'sentinel'

export interface EventFilters {
  category: EventCategory
  correlationId: string
  search: string
}

const MAX_EVENTS = 500

let events = $state<EventEntry[]>([])
let filters = $state<EventFilters>({ category: 'all', correlationId: '', search: '' })
let autoScroll = $state(true)
let lastSeenId = $state('')

let pollTimer: ReturnType<typeof setInterval> | null = null

// Map event type prefix to category
function eventCategory(type: string): EventCategory {
  if (type.startsWith('workflow.')) return 'lifecycle'
  if (type.startsWith('persona.')) return 'persona'
  if (type.startsWith('ai.')) return 'ai'
  if (type === 'feedback.generated' || type.startsWith('verdict.')) return 'feedback'
  if (type.startsWith('operator.')) return 'operator'
  if (type.startsWith('hint.')) return 'hints'
  if (type.startsWith('context.')) return 'context'
  if (type.startsWith('unhandled.')) return 'sentinel'
  return 'lifecycle'
}

// Color class for event category
export function eventColorClass(type: string): string {
  const cat = eventCategory(type)
  switch (cat) {
    case 'lifecycle': return 'text-purple-600'
    case 'persona': return type.includes('failed') ? 'text-red-500' : 'text-emerald-600'
    case 'ai': return 'text-cyan-600'
    case 'feedback': return 'text-amber-600'
    case 'operator': return 'text-blue-500'
    case 'hints': return 'text-teal-600'
    case 'context': return 'text-gray-500'
    case 'sentinel': return 'text-orange-500'
    default: return 'text-gray-500'
  }
}

function applyFilters(allEvents: EventEntry[], f: EventFilters): EventEntry[] {
  let result = allEvents

  if (f.category !== 'all') {
    result = result.filter(e => eventCategory(e.type) === f.category)
  }

  if (f.correlationId) {
    result = result.filter(e => e.correlation_id === f.correlationId)
  }

  if (f.search) {
    const term = f.search.toLowerCase()
    result = result.filter(e =>
      e.type.toLowerCase().includes(term) ||
      e.source?.toLowerCase().includes(term) ||
      e.id.toLowerCase().includes(term)
    )
  }

  return result
}

let filteredEvents = $derived(applyFilters(events, filters))

async function fetchEvents() {
  try {
    // @ts-ignore — Wails runtime
    if (!window.go?.main?.App?.ListEvents) return
    // @ts-ignore
    const result: EventEntry[] = await window.go.main.App.ListEvents('', MAX_EVENTS)
    if (!result) return

    // Find new events by comparing with lastSeenId
    if (lastSeenId && events.length > 0) {
      const newEvents = result.filter(e => {
        // Simple approach: events we don't already have by ID
        return !events.some(existing => existing.id === e.id)
      })
      if (newEvents.length > 0) {
        events = [...events, ...newEvents].slice(-MAX_EVENTS)
        lastSeenId = events[events.length - 1]?.id ?? ''
      }
    } else {
      events = result.slice(-MAX_EVENTS)
      lastSeenId = events[events.length - 1]?.id ?? ''
    }
  } catch {
    // Non-critical — dashboard store handles serverReachable
  }
}

function startPolling() {
  stopPolling()
  fetchEvents()
  pollTimer = setInterval(fetchEvents, 2000)
}

function stopPolling() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function setFilter(key: keyof EventFilters, value: string) {
  filters = { ...filters, [key]: value }
}

function setAutoScroll(value: boolean) {
  autoScroll = value
}

function clearEvents() {
  events = []
  lastSeenId = ''
}

export const eventsStore = {
  get events() { return filteredEvents },
  get allEvents() { return events },
  get filters() { return filters },
  get autoScroll() { return autoScroll },
  startPolling,
  stopPolling,
  setFilter,
  setAutoScroll,
  clearEvents,
  eventColorClass,
}
