import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { TemplateGallery } from '../../src/components/launchConfig'
import type { Template } from '../../src/types'

const tpl = (over: Partial<Template> = {}): Template => ({
  id: 'playwright', name: 'Playwright', description: 'E2E testing',
  tags: ['e2e'], language: '', cpu: 2, memory_mb: 2048, visibility: 'public',
  preinstalled: ['Chromium 131', '@playwright/test', 'Node 20', 'ffmpeg', 'xvfb'],
  has_server: true, default_port: 9323,
  ...over,
})

// The card renders a <Link>, so it needs a Router context.
function renderCard(t: Template) {
  return render(
    <MemoryRouter>
      <TemplateGallery templates={[t]} onPick={vi.fn()} />
    </MemoryRouter>,
  )
}

describe('template card', () => {
  it('shows what is preinstalled — the data was already on the wire', () => {
    renderCard(tpl())
    expect(screen.getByText(/Chromium 131/)).toBeInTheDocument()
    expect(screen.getByText(/@playwright\/test/)).toBeInTheDocument()
  })

  it('summarises the overflow rather than listing everything', () => {
    renderCard(tpl())
    expect(screen.getByText('+2')).toBeInTheDocument()
  })

  it('says in words that the template serves something openable', () => {
    renderCard(tpl())
    expect(screen.getByText(/opens a web app/i)).toBeInTheDocument()
  })

  it('says nothing about serving when the template has no server', () => {
    renderCard(tpl({ has_server: false, default_port: undefined }))
    expect(screen.queryByText(/opens a web app/i)).toBeNull()
  })

  it('shows resources and quotes no price', () => {
    renderCard(tpl())
    expect(screen.getByText(/2 vCPU/)).toBeInTheDocument()
    expect(screen.queryByText(/\/hr/)).toBeNull()
    expect(screen.queryByText(/\$/)).toBeNull()
  })

  it('survives a template with no preinstalled list', () => {
    renderCard(tpl({ preinstalled: undefined }))
    expect(screen.getByText('Playwright')).toBeInTheDocument()
  })
})
