import { describe, it, expect } from 'vitest'
import { starterFiles, starterStart } from '../../src/lib/launchStarters'
import type { Template } from '../../src/types'

// python-3.11's image has no matplotlib, and a starter that imports a missing
// module leaves a sandbox that never answers. The Python starter runs on the
// standard library alone, so it works on every Python template.
describe('python starter', () => {
  it('imports nothing outside the standard library', () => {
    const tpl: Template = { id: 'python-3.11', name: 'Python', description: '', cpu: 1, memory_mb: 512, visibility: 'public', language: 'python' }
    const py = starterFiles(tpl).find(f => f.path.endsWith('.py'))!.content
    const imports = [...py.matchAll(/^\s*(?:import|from)\s+([\w.]+)/gm)].map(m => m[1].split('.')[0])
    expect(imports.length).toBeGreaterThan(0)
    for (const mod of imports) expect(['os', 'csv', 'html', 'http', 'socketserver', 'collections']).toContain(mod)
  })
})

// With no starter, rust fell back to the terminal script, which downloads ttyd
// and has nothing to fall back on in an image with no curl, wget, node or
// python — the docker rust image — so the preview never answered.
describe('rust starter', () => {
  it('ships a crate that serves $PORT with no dependencies to fetch', () => {
    const tpl: Template = { id: 'rust', name: 'Rust', description: '', cpu: 2, memory_mb: 2048, visibility: 'public', language: 'rust' }
    const files = starterFiles(tpl)
    const cargo = files.find(f => f.path === 'Cargo.toml')!.content
    const main = files.find(f => f.path === 'main.rs')!.content
    expect(cargo).toContain('path = "main.rs"')
    expect(cargo).not.toMatch(/\[dependencies\][^[]*\S/)
    expect(main).toContain('env::var("PORT")')
  })

  // One 1 KB read left a proxied request's headers unread, the close reset the
  // connection, and the preview showed "App isn't responding yet" for good.
  it('reads the whole request head, and gives up on a silent client', () => {
    const main = starterFiles({ id: 'rust', name: 'Rust', description: '', cpu: 2, memory_mb: 2048, visibility: 'public', language: 'rust' })[0].content
    expect(main).toContain('b"\\r\\n\\r\\n"')
    expect(main).toContain('set_read_timeout')
  })
})

// "go" as a substring also matched mongodb and django, so they got a Go project.
describe('go starter', () => {
  const tpl = (id: string, language: string): Template => ({ id, name: id, description: '', cpu: 1, memory_mb: 512, visibility: 'public', language })

  it('goes to Go templates only', () => {
    expect(starterFiles(tpl('go', 'go')).map(f => f.path)).toEqual(['main.go', 'go.mod'])
    expect(starterFiles(tpl('golang-1.22', '')).map(f => f.path)).toEqual(['main.go', 'go.mod'])
    expect(starterFiles(tpl('mongodb', 'shell'))).toEqual([])
    expect(starterFiles(tpl('django', 'python')).map(f => f.path)).not.toContain('main.go')
  })
})

describe('node starter', () => {
  it('declares UTF-8, since it serves an emoji', () => {
    const tpl: Template = { id: 'node', name: 'Node', description: '', cpu: 1, memory_mb: 512, visibility: 'public', language: 'node' }
    expect(starterFiles(tpl)[0].content).toContain('text/html; charset=utf-8')
  })

  // The starter page promises an edit shows up on save.
  it('starts in watch mode', () => {
    const tpl: Template = { id: 'node', name: 'Node', description: '', cpu: 1, memory_mb: 512, visibility: 'public', language: 'node' }
    expect(starterStart(tpl)).toBe('node --watch server.js')
  })
})
