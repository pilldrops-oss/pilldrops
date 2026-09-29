// Which tabs a sandbox gets. The agent predicate reads manifest kind only, the
// same field the API gate reads; matching tags would show an Agent tab whose
// routes 404 (openclaw is tagged "agent" but kind=app).

/** TabTemplateMeta is the slice of the launch manifest these decisions read. */
export type TabTemplateMeta = {
  kind?: string
  outcome?: string
  /** Present when the template ships an agent CLI; makes Model/Keys meaningful. */
  agent?: unknown
  /** Deliberately unread; declared so an edit that reaches for it is visibly wrong. */
  tags?: string[]
}

export type TabGroup<T extends string = string> = {
  id: string
  label: string
  members: Array<{ id: T; label: string }>
}

/**
 * agentTabsVisible reports whether this sandbox has an agent surface.
 * Undefined meta is false: show fewer tabs while loading, not more.
 */
export function agentTabsVisible(meta: TabTemplateMeta | null | undefined): boolean {
  return meta?.kind === 'agent'
}

/** previewLabel names the Preview tab after the manifest's outcome. */
export function previewLabel(outcome?: string): string {
  switch (outcome) {
    case 'screen': return 'Screen'
    case 'report': return 'Report'
    case 'verdict': return 'Verdict'
    case 'endpoint': return 'Endpoint'
    case 'connection': return 'Connection'
    default: return 'Preview'
  }
}

/**
 * tabGroupsFor builds the tab strip: five groups with sub-strips for agent
 * sandboxes, one group per tab (flat) for everything else.
 */
export function tabGroupsFor(meta: TabTemplateMeta | null | undefined): TabGroup[] {
  const settings: TabGroup['members'] = [
    ...(meta?.agent
      ? [
          { id: 'model', label: 'Model' },
          { id: 'keys', label: 'Keys' },
          { id: 'integrations', label: 'Integrations' },
          { id: 'channels', label: 'Channels' },
        ]
      : []),
    { id: 'info', label: 'Info' },
  ]

  if (agentTabsVisible(meta)) {
    return [
      { id: 'agent', label: 'Agent', members: [{ id: 'agent', label: 'Agent' }] },
      { id: 'timeline', label: 'Timeline', members: [{ id: 'timeline', label: 'Timeline' }] },
      {
        id: 'workspace', label: 'Workspace',
        members: [
          { id: 'shell', label: 'Terminal' },
          { id: 'terminal', label: 'Exec' },
          { id: 'files', label: 'Files' },
          { id: 'preview', label: previewLabel(meta?.outcome) },
          { id: 'logs', label: 'Logs' },
        ],
      },
      { id: 'connect', label: 'Connect', members: [{ id: 'connect', label: 'Connect' }] },
      { id: 'settings', label: 'Settings', members: settings },
    ]
  }

  const flat: TabGroup['members'] = [
    { id: 'shell', label: 'Terminal' },
    { id: 'terminal', label: 'Exec' },
    { id: 'files', label: 'Files' },
    { id: 'preview', label: previewLabel(meta?.outcome) },
    { id: 'logs', label: 'Logs' },
    ...settings,
  ]
  return flat.map(t => ({ id: t.id, label: t.label, members: [t] }))
}
