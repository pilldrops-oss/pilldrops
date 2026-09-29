import { describe, it, expect } from 'vitest'

// Retired routes must still resolve: React Router renders nothing for a dead link, so a
// bookmarked /history or /logs would show a blank page.

const app = import.meta.glob('../../src/App.tsx', { query: '?raw', import: 'default', eager: true })
const appSrc = Object.values(app)[0] as string

const sources = import.meta.glob('../../src/**/*.tsx', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

describe('retired routes still resolve', () => {
  it('reads App.tsx', () => {
    expect(appSrc).toContain('<Route')
  })

  it.each([
    ['history', '/sandboxes?tab=past'],
    ['logs', '/sandboxes'],
  ])('/%s redirects to %s', (path, target) => {
    const re = new RegExp(`path="${path}"[^>]*<Navigate to="${target.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}"`)
    expect(
      re.test(appSrc.replace(/\s+/g, ' ')),
      `/${path} has no redirect to ${target} — the URL is bookmarked and linked, and would render a blank page`,
    ).toBe(true)
  })

  it('no longer routes the folded pages to their own components', () => {
    const flat = appSrc.replace(/\s+/g, ' ')
    for (const gone of ['element={<History />}', 'element={<Logs />}']) {
      expect(flat, `${gone} should be gone — the page is now hosted elsewhere`).not.toContain(gone)
    }
  })
})

describe('nothing links to a removed nav entry', () => {
  it('has no navigate() or NavLink to /billing, /history or /logs outside App.tsx', () => {
    // Redirects keep old URLs alive; in-app links should point at the real page.
    const offenders: string[] = []
    for (const [path, src] of Object.entries(sources)) {
      if (path.includes('App.tsx') || path.includes('.test.')) continue
      for (const m of src.matchAll(/(?:navigate\(|to=)["'`]\/?(history|logs)["'`]/g)) {
        offenders.push(`${path}: ${m[0]}`)
      }
    }
    expect(offenders, `these still point at a folded route:\n${offenders.join('\n')}`).toEqual([])
  })

  it('the sidebar no longer offers the folded entries', () => {
    const shell = sources['../../src/components/Shell.tsx']
    expect(shell).toBeTruthy()
    expect(shell).not.toContain('to="/logs"')
    expect(shell).not.toContain('to="/history"')
    expect(shell).toContain('to="/usage"')
  })
})

// The open-source core takes no money, so a Billing tab reappearing means a payment path
// was wired back in.
describe('no purchase surface', () => {
  it('App.tsx has no /billing route', () => {
    expect(appSrc).not.toContain('path="billing"')
  })

  it('the sidebar offers Usage and nothing to buy', () => {
    const shell = sources['../../src/components/Shell.tsx']
    expect(shell).toBeTruthy()
    expect(shell).toContain('to="/usage"')
    expect(shell).not.toContain('to="/billing"')
  })
})

// /deploy and /templates stay routed (the launch flow and marketing deep links use them)
// but are no longer top-level nav.
describe('catalog and templates are no longer primary nav', () => {
  const shell = sources['../../src/components/Shell.tsx']

  it('the sidebar has no Catalog or Recipes entry — Build is the front door', () => {
    expect(shell).toBeTruthy()
    expect(shell).not.toMatch(/<NavLink\s+to="\/deploy"/)
    expect(shell).not.toMatch(/<NavLink\s+to="\/recipes"/)
  })

  it('nothing links the deleted recipes page', () => {
    for (const [name, src] of Object.entries(sources)) {
      expect(src, `${name} links /recipes`).not.toContain('to="/recipes"')
    }
  })

  it('Templates moved out of the primary group', () => {
    // It sits with Account, not alongside Sandboxes.
    expect(shell).toContain('to="/templates"')
    const keysAt = shell.indexOf('to="/keys"')
    const templatesAt = shell.indexOf('to="/templates"')
    expect(templatesAt).toBeGreaterThan(shell.indexOf('to="/usage"'))
    expect(templatesAt).toBeLessThan(keysAt)
  })

  it('launching still reaches the deploy flow', () => {
    // Without this button the catalogue is unreachable from the nav.
    const sandboxes = sources['../../src/pages/Sandboxes.tsx']
    expect(sandboxes).toBeTruthy()
    expect(sandboxes).toMatch(/navigate\(\s*['"]\/deploy['"]/)
  })

  it('/deploy and /templates are still routed', () => {
    const flat = appSrc.replace(/\s+/g, ' ')
    expect(flat).toContain('path="deploy"')
    expect(flat).toContain('path="templates"')
  })
})

describe('the customer home', () => {
  // Build is a hosted-product surface outside the open-source build, so the index forwards
  // to the sandbox table.
  it('sends the customer index to the sandbox table', () => {
    const flat = appSrc.replace(/\s+/g, ' ')
    expect(flat).toMatch(/<Route index element=\{<Navigate to=\{IS_ADMIN \? '\/overview' : '\/sandboxes'\} replace \/>\}/)
  })

  it('does not route or import Build at all', () => {
    expect(appSrc).not.toMatch(/<Build \/>/)
    expect(appSrc).not.toMatch(/from '\.\/pages\/Build'/)
  })

  it('has no Build entry in the primary nav', () => {
    const shell = sources['../../src/components/Shell.tsx']
    expect(shell).toBeTruthy()
    expect(shell).not.toMatch(/>\s*Build\s*</)
  })

  it('gives the sandbox table its own route rather than the index', () => {
    const flat = appSrc.replace(/\s+/g, ' ')
    expect(flat).toMatch(/path="sandboxes"[^>]*element=\{<Sandboxes \/>\}/)
  })

  it('leaves no competing describe-a-need box on the catalogue', () => {
    const deploy = sources['../../src/pages/Deploy.tsx'] ?? ''
    expect(deploy).not.toMatch(/<AiCatalogPrompt\b/)
  })
})

// Projects is only populated once a run has been kept, so it is reached from the flow, not the nav.
describe('projects is reachable from the flow, not the nav', () => {
  it('has no Projects entry in the sidebar', () => {
    const shell = sources['../../src/components/Shell.tsx']
    expect(shell).toBeTruthy()
    expect(shell).not.toMatch(/<NavLink\s+to="\/projects"/)
  })

  it('/projects is still routed', () => {
    const flat = appSrc.replace(/\s+/g, ' ')
    expect(flat).toContain('path="projects"')
  })
})

describe('the catalogue answers to one name', () => {
  it('breadcrumb label for /deploy says Catalog', async () => {
    const { labelFor } = await import('../../src/lib/navLabels')
    expect(labelFor('deploy', false)).toBe('Catalog')
  })

  it('the gallery H1 says Catalog', () => {
    const deploy = sources['../../src/pages/Deploy.tsx']
    expect(deploy).toBeTruthy()
    expect(deploy).toContain('<h1 className="h1">Catalog</h1>')
    expect(deploy).not.toContain('Deploy a sandbox</h1>')
  })
})
