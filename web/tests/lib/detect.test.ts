import { describe, it, expect } from 'vitest'
import { detectProject } from '../../src/lib/detect'

describe('detectProject — which manifest is the project', () => {
  it('a Python repo with JS tooling is Python, not Node', () => {
    // Shaped like django/django: a tooling-only package.json beside the real pyproject.toml.
    const det = detectProject([
      { path: 'package.json', content: JSON.stringify({
        scripts: { test: 'grunt test', biome: 'biome ci' },
        devDependencies: { '@biomejs/biome': '1', grunt: '1' },
      }) },
      { path: 'pyproject.toml', content: '[project]\nname = "django"\n' },
    ])
    expect(det?.templateId).not.toBe('node')
    expect(det?.label).toMatch(/python/i)
  })

  it('a real JS app with a lone package.json is still Node', () => {
    const det = detectProject([
      { path: 'package.json', content: JSON.stringify({
        scripts: { start: 'node server.js' }, dependencies: { express: '4' },
      }) },
    ])
    expect(det?.templateId).toBe('node')
  })

  it('JS tooling ALONE is still a Node project', () => {
    const det = detectProject([
      { path: 'package.json', content: JSON.stringify({ scripts: { test: 'jest' } }) },
    ])
    expect(det?.templateId).toBe('node')
  })

  it('a full-stack repo that declares how to run itself stays Node', () => {
    const det = detectProject([
      { path: 'package.json', content: JSON.stringify({
        workspaces: ['frontend'], scripts: { dev: 'bun run --filter frontend dev' },
      }) },
      { path: 'pyproject.toml', content: '[project]\nname = "app"\n' },
    ])
    expect(det?.templateId).toBe('node')
  })
})

describe('detectProject — a start command must name a file that exists', () => {
  it('a FastAPI LIBRARY gets no start command', () => {
    // Shaped like fastapi/fastapi: the library itself, with no main.py to launch.
    const det = detectProject([
      { path: 'pyproject.toml', content: '[project]\nname = "fastapi"\ndependencies = ["fastapi"]\n' },
    ])
    expect(det?.start, 'must not invent an entry point').toBeUndefined()
    // Still identified, so the user is told what it is rather than nothing.
    expect(det?.label).toMatch(/fastapi/i)
  })

  it('FastAPI maps to the real catalogue id, not the python-3.11 alias', () => {
    const det = detectProject([
      { path: 'requirements.txt', content: 'fastapi\n' },
    ])
    expect(det?.templateId).toBe('code-interpreter')
  })

  it('a FastAPI APP with a real entry point gets one', () => {
    const det = detectProject([
      { path: 'requirements.txt', content: 'fastapi\nuvicorn\n' },
      { path: 'main.py', content: 'from fastapi import FastAPI\napp = FastAPI()\n' },
    ])
    expect(det?.start).toContain('uvicorn main:app')
  })

  it('names the module where the app actually is, not where it is assumed to be', () => {
    const det = detectProject([
      { path: 'requirements.txt', content: 'fastapi\n' },
      { path: 'src/server.py', content: 'from fastapi import FastAPI\napp = FastAPI()\n' },
    ])
    expect(det?.start).toContain('uvicorn src.server:app')
  })

  it('Flask already worked this way and still does', () => {
    const noApp = detectProject([{ path: 'requirements.txt', content: 'flask\n' }])
    expect(noApp?.start).toBeUndefined()
    const withApp = detectProject([
      { path: 'requirements.txt', content: 'flask\n' },
      { path: 'app.py', content: 'from flask import Flask\napp = Flask(__name__)\n' },
    ])
    expect(withApp?.start).toContain('app.py')
  })
})
