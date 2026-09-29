import { describe, it, expect, beforeEach, vi } from 'vitest'
import {
  saveThread, loadThread, clearThread, describeAge, onThreadCleared,
  activeThreadId, switchToThread, onThreadSwitched, threadTitle,
} from '../../src/lib/threadStore'
import { createThread, patchThread, ApiError } from '../../src/api'

// The store syncs to the threads API behind every local write.
vi.mock('../../src/api', async importOriginal => {
  const real = await importOriginal<typeof import('../../src/api')>()
  return { ...real, createThread: vi.fn(), patchThread: vi.fn() }
})

const createMock = vi.mocked(createThread)
const patchMock = vi.mocked(patchThread)

beforeEach(() => {
  localStorage.clear()
  vi.restoreAllMocks()
  createMock.mockReset().mockResolvedValue({ id: 'th_new', title: 't', created_at: 0, updated_at: 0 })
  patchMock.mockReset().mockResolvedValue({ id: 'th_new', title: 't', created_at: 0, updated_at: 0 })
})

describe('saveThread / loadThread', () => {
  it('round-trips a transcript', () => {
    saveThread([{ kind: 'you', text: 'hi' }])
    expect(loadThread()?.turns).toEqual([{ kind: 'you', text: 'hi' }])
  })

  it('stores when it was written, so the reader can say how stale it is', () => {
    saveThread([{ kind: 'you' }])
    expect(typeof loadThread()?.at).toBe('number')
  })

  it('an empty thread clears rather than storing nothing', () => {
    saveThread([{ kind: 'you' }])
    saveThread([])
    expect(loadThread()).toBeNull()
  })

  it('caps what it keeps — a conversation is not an archive', () => {
    saveThread(Array.from({ length: 200 }, (_, i) => ({ n: i })))
    const turns = loadThread()?.turns ?? []
    expect(turns.length).toBeLessThanOrEqual(60)
    // Keeps the most recent, not the first.
    expect(turns[turns.length - 1]).toEqual({ n: 199 })
  })
})

describe('loadThread — degrades rather than crashes', () => {
  it.each([
    ['not json at all', 'not json'],
    ['a bare array', '[]'],
    ['an object with no turns', '{"at":1}'],
    ['turns of the wrong type', '{"at":1,"turns":"nope"}'],
    ['no timestamp', '{"turns":[{"kind":"you"}]}'],
    ['an empty transcript', '{"at":1,"turns":[]}'],
  ])('%s → null', (_label, raw) => {
    localStorage.setItem('pilldrops.build.thread', raw)
    expect(loadThread()).toBeNull()
  })

  it('survives storage throwing outright', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError: storage disabled')
    })
    expect(loadThread()).toBeNull()
  })
})

describe('saveThread — never interrupts the user', () => {
  it('swallows a quota or security failure', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError')
    })
    expect(() => saveThread([{ kind: 'you' }])).not.toThrow()
  })
})

describe('clearThread', () => {
  it('removes a stored transcript', () => {
    saveThread([{ kind: 'you' }])
    clearThread()
    expect(loadThread()).toBeNull()
  })
})

describe('describeAge', () => {
  const t0 = 1_700_000_000_000
  it.each([
    [t0, 'just now'],
    [t0 - 5 * 60_000, '5m ago'],
    [t0 - 90 * 60_000, '2h ago'],
    [t0 - 50 * 3_600_000, '2d ago'],
  ])('reads naturally', (at, expected) => {
    expect(describeAge(at, t0)).toBe(expected)
  })

  it('never renders a negative age from a clock that moved backwards', () => {
    // Timezone changes and NTP corrections both move the clock backwards.
    expect(describeAge(t0 + 60_000, t0)).toBe('just now')
  })
})

// Build is the index route, so "New" never remounts it; clearing must be an event the
// mounted view hears, not just a storage write.
describe('clearThread announces itself', () => {
  it('notifies subscribers so a mounted view can reset', () => {
    const seen: number[] = []
    const off = onThreadCleared(() => seen.push(1))
    saveThread([{ kind: 'you' }])
    clearThread()
    expect(seen).toHaveLength(1)
    off()
    clearThread()
    expect(seen).toHaveLength(1)
  })

  it('survives a listener that throws, so one bad view cannot strand the rest', () => {
    const seen: number[] = []
    const offBad = onThreadCleared(() => { throw new Error('boom') })
    const offGood = onThreadCleared(() => seen.push(1))
    expect(() => clearThread()).not.toThrow()
    expect(seen).toHaveLength(1)
    offBad(); offGood()
  })
})

describe('threadTitle', () => {
  it('is the user’s own first message, capped', () => {
    expect(threadTitle([{ kind: 'say', text: 'hello' }, { kind: 'you', text: '  a python api  ' }])).toBe('a python api')
    expect(threadTitle([{ kind: 'you', text: 'x'.repeat(200) }])).toHaveLength(80)
  })

  it('is empty when the user has said nothing — the server names it instead', () => {
    expect(threadTitle([{ kind: 'say', text: 'welcome' }])).toBe('')
  })
})

describe('server sync', () => {
  it('the first save CREATES, titled from the first prompt, and adopts the id', async () => {
    saveThread([{ kind: 'you', text: 'a python api' }])
    await vi.waitFor(() => expect(activeThreadId()).toBe('th_new'))
    expect(createMock).toHaveBeenCalledTimes(1)
    const [title, turns] = createMock.mock.calls[0]
    expect(title).toBe('a python api')
    expect(JSON.parse(turns)).toEqual([{ kind: 'you', text: 'a python api' }])
  })

  it('later saves PATCH the adopted thread instead of creating twins', async () => {
    saveThread([{ kind: 'you', text: 'first' }])
    await vi.waitFor(() => expect(activeThreadId()).toBe('th_new'))
    saveThread([{ kind: 'you', text: 'first' }, { kind: 'say', text: 'reply' }])
    await vi.waitFor(() => expect(patchMock).toHaveBeenCalled())
    expect(createMock).toHaveBeenCalledTimes(1)
    expect(patchMock).toHaveBeenCalledWith('th_new', { turns: expect.stringContaining('reply') })
  })

  it('a thread deleted elsewhere (404) is let go — the next save gets a new home', async () => {
    saveThread([{ kind: 'you', text: 'first' }])
    await vi.waitFor(() => expect(activeThreadId()).toBe('th_new'))
    patchMock.mockRejectedValueOnce(
      new ApiError({ status: 404, method: 'PATCH', path: '/v1/threads/th_new', message: 'not found' }),
    )
    saveThread([{ kind: 'you', text: 'first' }, { kind: 'say', text: 'more' }])
    await vi.waitFor(() => expect(activeThreadId()).toBeNull())
  })

  it('a server outage is silent and keeps the id for when it returns', async () => {
    saveThread([{ kind: 'you', text: 'first' }])
    await vi.waitFor(() => expect(activeThreadId()).toBe('th_new'))
    patchMock.mockRejectedValueOnce(new Error('network down'))
    expect(() => saveThread([{ kind: 'you', text: 'first' }, { kind: 'say', text: 'x' }])).not.toThrow()
    await vi.waitFor(() => expect(patchMock).toHaveBeenCalled())
    expect(activeThreadId()).toBe('th_new')
  })
})

describe('switchToThread', () => {
  it('replaces the local transcript, adopts the id, and announces itself', () => {
    const seen: number[] = []
    const off = onThreadSwitched(() => seen.push(1))
    switchToThread('th_9', JSON.stringify([{ kind: 'you', text: 'older work' }]), 1_700_000_000)
    expect(activeThreadId()).toBe('th_9')
    expect(loadThread()?.turns).toEqual([{ kind: 'you', text: 'older work' }])
    // The stored age is the thread's own updated time, not when it was opened.
    expect(loadThread()?.at).toBe(1_700_000_000_000)
    expect(seen).toHaveLength(1)
    off()
  })

  it('an unreadable transcript opens empty rather than not at all', () => {
    saveThread([{ kind: 'you', text: 'current' }])
    switchToThread('th_bad', 'not json', 0)
    expect(activeThreadId()).toBe('th_bad')
    expect(loadThread()).toBeNull()
  })
})

describe('clearThread detaches from the server copy', () => {
  it('＋ New leaves the old conversation on the server and starts unattached', async () => {
    saveThread([{ kind: 'you', text: 'first' }])
    await vi.waitFor(() => expect(activeThreadId()).toBe('th_new'))
    clearThread()
    expect(activeThreadId()).toBeNull()
  })
})
