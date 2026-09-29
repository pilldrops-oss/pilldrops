import { describe, it, expect } from 'vitest'
import { languageIdFor, LANGUAGE_LABEL, type FileLang } from '../../src/components/fileLang'

describe('languageIdFor', () => {
  it.each([
    ['main.py', 'python'],
    ['app.js', 'javascript'],
    ['app.mjs', 'javascript'],
    ['index.tsx', 'javascript'],
    ['package.json', 'json'],
    ['README.md', 'markdown'],
    ['index.html', 'html'],
    ['theme.css', 'css'],
    ['docker-compose.yml', 'yaml'],
    ['main.go', 'go'],
    ['lib.rs', 'rust'],
    ['entrypoint.sh', 'shell'],
    ['schema.sql', 'sql'],
    ['Cargo.toml', 'toml'],
    ['pom.xml', 'xml'],
    ['main.c', 'clike'],
    ['App.java', 'clike'],
    ['script.rb', 'ruby'],
    ['index.php', 'php'],
  ])('maps %s to %s', (name, expected) => {
    expect(languageIdFor(name)).toBe(expected)
  })

  it.each([
    ['Dockerfile', 'dockerfile'],
    ['Dockerfile.prod', 'dockerfile'],
    ['Makefile', 'plain'],
  ])('handles the extensionless file %s', (name, expected) => {
    expect(languageIdFor(name)).toBe(expected)
  })

  it('falls back to plain text for unknown extensions', () => {
    expect(languageIdFor('data.bin')).toBe('plain')
    expect(languageIdFor('LICENSE')).toBe('plain')
    expect(languageIdFor('')).toBe('plain')
  })

  it('ignores case and directory components', () => {
    expect(languageIdFor('src/deep/nested/MAIN.PY')).toBe('python')
    expect(languageIdFor('/abs/path/App.JSX')).toBe('javascript')
  })

  it('uses the last extension on multi-dotted names', () => {
    expect(languageIdFor('bundle.min.js')).toBe('javascript')
    expect(languageIdFor('archive.tar.gz')).toBe('plain')
  })

  it('labels every language it can return', () => {
    const ids: FileLang[] = [
      'python', 'javascript', 'json', 'markdown', 'html', 'css', 'yaml',
      'go', 'rust', 'shell', 'sql', 'toml', 'xml', 'clike', 'ruby', 'php',
      'dockerfile', 'plain',
    ]
    for (const id of ids) {
      expect(LANGUAGE_LABEL[id], `no label for ${id}`).toBeTruthy()
    }
  })
})
