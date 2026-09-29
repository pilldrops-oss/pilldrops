import { describe, it, expect } from 'vitest'
import { joinPath, baseName, depthOf, ROOT } from '../../src/lib/filePath'

describe('joinPath', () => {
  it('keeps the root implicit', () => {
    // "./src" and "src" as separate Record keys leave a tree expanded and collapsed at once.
    expect(joinPath(ROOT, 'src')).toBe('src')
    expect(joinPath('', 'src')).toBe('src')
  })

  it('joins nested directories', () => {
    expect(joinPath('src', 'main.py')).toBe('src/main.py')
    expect(joinPath('src/lib', 'util.ts')).toBe('src/lib/util.ts')
  })

  it('does not double the separator', () => {
    expect(joinPath('src/', 'main.py')).toBe('src/main.py')
    expect(joinPath('src//', 'main.py')).toBe('src/main.py')
  })
})

describe('baseName', () => {
  it('returns the last segment', () => {
    expect(baseName('src/lib/util.ts')).toBe('util.ts')
    expect(baseName('main.py')).toBe('main.py')
  })

  it('survives trailing slashes', () => {
    expect(baseName('src/lib/')).toBe('lib')
  })
})

describe('depthOf', () => {
  it('measures nesting for indentation', () => {
    expect(depthOf(ROOT)).toBe(0)
    expect(depthOf('src')).toBe(1)
    expect(depthOf('src/lib/util.ts')).toBe(3)
  })
})
