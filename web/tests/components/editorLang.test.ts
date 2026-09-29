import { describe, it, expect } from 'vitest'
import { editorLangFor, langForTemplate } from '../../src/components/editorLang'

describe('editorLangFor', () => {
  it('treats the JS family, including its config and markup, as javascript', () => {
    for (const p of ['a.js', 'a.jsx', 'a.ts', 'a.tsx', 'a.mjs', 'a.cjs',
                     'a.json', 'a.html', 'a.htm', 'a.css', 'a.scss']) {
      expect(editorLangFor(p)).toBe('javascript')
    }
  })

  it('falls back to python for everything else', () => {
    for (const p of ['a.py', 'a.go', 'a.rs', 'Dockerfile', 'README.md', 'noext']) {
      expect(editorLangFor(p)).toBe('python')
    }
  })
})

describe('langForTemplate', () => {
  it('recognises the node/typescript family', () => {
    for (const t of ['node-20', 'nextjs', 'typescript', 'deno', 'bun']) {
      expect(langForTemplate(t)).toBe('javascript')
    }
  })

  it('defaults to python', () => {
    expect(langForTemplate('python-3.11')).toBe('python')
    expect(langForTemplate('base')).toBe('python')
  })
})
