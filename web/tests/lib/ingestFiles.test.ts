import { describe, it, expect } from 'vitest'
import { isIngestable, stripCommonRoot, mergeByPath, MAX_FILE } from '../../src/lib/ingestFiles'

describe('isIngestable', () => {
  it('drops vendored and generated directories', () => {
    for (const p of ['node_modules/x/i.js', 'a/node_modules/x.js', '.git/config',
                     'dist/b.js', 'build/b.js', '.next/x.js', 'target/x.rs',
                     '.venv/lib.py', '__pycache__/a.pyc', '__MACOSX/._a']) {
      expect(isIngestable(p), p).toBe(false)
    }
  })

  it('drops binaries by extension', () => {
    for (const p of ['logo.png', 'a.PDF', 'f.woff2', 'v.mp4', 'lib.so', 'x.zip']) {
      expect(isIngestable(p), p).toBe(false)
    }
  })

  it('keeps source, including paths that merely contain a skipped word', () => {
    for (const p of ['src/index.js', 'main.py', 'rebuild.md', 'my-dist-tool/a.ts']) {
      expect(isIngestable(p), p).toBe(true)
    }
  })

  it('drops anything over the size cap', () => {
    expect(isIngestable('big.js', MAX_FILE + 1)).toBe(false)
    expect(isIngestable('ok.js', MAX_FILE)).toBe(true)
  })
})

describe('stripCommonRoot', () => {
  it('removes a single shared wrapper directory', () => {
    const out = stripCommonRoot([
      { path: 'my-app/src/index.js', content: '' },
      { path: 'my-app/package.json', content: '' },
    ])
    expect(out.map(f => f.path)).toEqual(['src/index.js', 'package.json'])
  })

  it('leaves files alone when they do not all share a root', () => {
    const files = [
      { path: 'my-app/src/index.js', content: '' },
      { path: 'README.md', content: '' },
    ]
    expect(stripCommonRoot(files)).toEqual(files)
  })

  it('leaves a lone file alone, since one path is not evidence of a wrapper', () => {
    const files = [{ path: 'my-app/index.js', content: '' }]
    expect(stripCommonRoot(files)).toEqual(files)
  })
})

describe('mergeByPath', () => {
  it('lets a re-uploaded file replace the old one and keeps the rest', () => {
    const out = mergeByPath(
      [{ path: 'a.js', content: 'old' }, { path: 'b.js', content: 'b' }],
      [{ path: 'a.js', content: 'new' }, { path: 'c.js', content: 'c' }],
    )
    expect(out).toEqual([
      { path: 'a.js', content: 'new' },
      { path: 'b.js', content: 'b' },
      { path: 'c.js', content: 'c' },
    ])
  })
})
