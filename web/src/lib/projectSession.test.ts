import { describe, it, expect, vi, beforeEach } from 'vitest'
import { get } from 'svelte/store'
import type { Session, SessionGroup } from './types'

// Entering a project must never create anything: with a remembered session it
// opens it, and with none it parks the group on the blank composer.
vi.mock('./api', () => ({
  createSession: vi.fn(() => {
    throw new Error('createSession must not be called by openProject')
  }),
  updateSessionGroup: vi.fn(async () => ({})),
}))

import * as api from './api'
import {
  activeSessionId,
  view,
  sessions,
  sessionGroups,
  pendingGroupId,
  lastSessionForProject,
  openProject,
  noteProjectSessionOpen,
} from './stores'

const sess = (id: string, updated_at: string) => ({ id, updated_at }) as unknown as Session

const project = (over: Partial<SessionGroup> = {}): SessionGroup =>
  ({ id: 'g1', name: 'Work', session_ids: ['s-old', 's-new'], working_dir: '/w', ...over }) as SessionGroup

beforeEach(() => {
  vi.clearAllMocks()
  activeSessionId.set(null)
  view.set('chat')
  pendingGroupId.set('')
  sessions.set([])
  sessionGroups.set([])
})

describe('lastSessionForProject', () => {
  it('prefers the remembered session while it is still live', () => {
    sessions.set([sess('s-old', '2026-01-01T00:00:00Z'), sess('s-new', '2026-02-01T00:00:00Z')])
    expect(lastSessionForProject(project({ last_session_id: 's-old' }))).toBe('s-old')
  })

  it('falls back to the newest member when the remembered id was deleted', () => {
    sessions.set([sess('s-old', '2026-01-01T00:00:00Z'), sess('s-new', '2026-02-01T00:00:00Z')])
    expect(lastSessionForProject(project({ last_session_id: 'gone' }))).toBe('s-new')
  })

  it('trusts the remembered session before the session list has arrived', () => {
    sessions.set([])
    expect(lastSessionForProject(project({ last_session_id: 's-old' }))).toBe('s-old')
  })

  it('answers null when no member is live', () => {
    sessions.set([sess('elsewhere', '2026-02-01T00:00:00Z')])
    expect(lastSessionForProject(project())).toBeNull()
  })
})

describe('openProject', () => {
  it('reopens the remembered session without creating one', () => {
    sessionGroups.set([project({ last_session_id: 's-old' })])
    sessions.set([sess('s-old', '2026-01-01T00:00:00Z'), sess('s-new', '2026-02-01T00:00:00Z')])
    openProject('g1')
    expect(get(activeSessionId)).toBe('s-old')
    expect(get(view)).toBe('chat')
    expect(api.createSession).not.toHaveBeenCalled()
  })

  it('parks the group on the blank composer when the project has no session', () => {
    sessionGroups.set([project({ session_ids: [], last_session_id: undefined })])
    openProject('g1')
    expect(get(activeSessionId)).toBeNull()
    expect(get(pendingGroupId)).toBe('g1')
    expect(api.createSession).not.toHaveBeenCalled()
  })
})

describe('noteProjectSessionOpen', () => {
  it('persists the open session on its project once', () => {
    sessionGroups.set([project({ last_session_id: 's-new' })])
    noteProjectSessionOpen('s-old')
    expect(get(sessionGroups)[0].last_session_id).toBe('s-old')
    expect(api.updateSessionGroup).toHaveBeenCalledWith('g1', { last_session_id: 's-old' })
    noteProjectSessionOpen('s-old')
    expect(api.updateSessionGroup).toHaveBeenCalledTimes(1)
  })
})
