import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import TemplateDetail from '../../src/pages/TemplateDetail'
import * as api from '../../src/api'
import type { Template } from '../../src/types'

const playwright: Template = {
  id: 'playwright', name: 'Playwright', description: 'E2E testing with Chromium.',
  tags: ['e2e', 'browser'], language: '', cpu: 2, memory_mb: 2048, visibility: 'public',
  preinstalled: ['Chromium 131', '@playwright/test', 'Node 20'],
  has_server: true, server_desc: 'HTML test report', default_port: 9323,
  setup_suggestion: ['npx playwright install --with-deps'],
}

function renderAt(id: string) {
  return render(
    <MemoryRouter initialEntries={[`/t/${id}`]}>
      <Routes><Route path="/t/:id" element={<TemplateDetail />} /></Routes>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  vi.spyOn(api, 'listTemplates').mockResolvedValue([playwright])
})

describe('TemplateDetail', () => {
  it('names the template and what it is for', async () => {
    renderAt('playwright')
    expect(await screen.findByRole('heading', { name: /playwright/i })).toBeInTheDocument()
    expect(screen.getByText(/E2E testing with Chromium/)).toBeInTheDocument()
  })

  it('lists everything inside the image, not a truncated sample', async () => {
    renderAt('playwright')
    await waitFor(() => expect(screen.getByText('Chromium 131')).toBeInTheDocument())
    expect(screen.getByText('@playwright/test')).toBeInTheDocument()
    expect(screen.getByText('Node 20')).toBeInTheDocument()
  })

  it('describes what it serves', async () => {
    renderAt('playwright')
    expect(await screen.findByText(/HTML test report/)).toBeInTheDocument()
  })

  it('offers a launch path', async () => {
    renderAt('playwright')
    expect(await screen.findByRole('link', { name: /launch/i })).toBeInTheDocument()
  })

  it('gives the CLI equivalent', async () => {
    renderAt('playwright')
    await waitFor(() => expect(screen.getByText(/pilldrops create/)).toBeInTheDocument())
  })

  it('says so plainly when the template does not exist', async () => {
    renderAt('does-not-exist')
    expect(await screen.findByText(/couldn’t find that template/i)).toBeInTheDocument()
  })
})
