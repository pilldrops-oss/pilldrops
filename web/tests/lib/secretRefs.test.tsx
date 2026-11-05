import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { composeCreateBody } from '../../src/lib/launchBody'
import { VaultSecretPicker } from '../../src/components/launch/panels'
import * as api from '../../src/api'
import type { Template } from '../../src/types'

vi.mock('../../src/api', () => ({ listSecrets: vi.fn(), absolutePreviewUrl: vi.fn() }))

const tpl = { id: 'node', has_server: false } as Template

describe('vault secrets at launch', () => {
  beforeEach(() => {
    vi.mocked(api.listSecrets).mockReset().mockResolvedValue([
      { name: 'DB_URL', created_at: '', updated_at: '' },
      { name: 'ALWAYS', auto_inject: true, created_at: '', updated_at: '' },
    ])
  })

  it('sends the picked names as secret_refs, for every kind', () => {
    for (const kind of ['web', 'shell', 'test', 'service', 'gui'] as const) {
      const body = composeCreateBody({
        tpl, kind, testCmd: '', serveReport: true, start: 'node a.js', port: '8000',
        envList: [], secretRefs: ['DB_URL'], previewPort: 8000,
      })
      expect(body.secret_refs, kind).toEqual(['DB_URL'])
    }
  })

  it('omits secret_refs when none are picked', () => {
    const body = composeCreateBody({
      tpl, kind: 'web', testCmd: '', serveReport: true, start: 'node a.js', port: '8000',
      envList: [], previewPort: 8000,
    })
    expect(body).not.toHaveProperty('secret_refs', expect.anything())
  })

  it('lists vault secrets and toggles a reference; auto-injected ones are fixed on', async () => {
    const onChange = vi.fn()
    render(<VaultSecretPicker selected={[]} onChange={onChange} />)
    const db = await screen.findByRole('checkbox', { name: /DB_URL/ })
    await userEvent.click(db)
    expect(onChange).toHaveBeenCalledWith(['DB_URL'])
    const always = screen.getByRole('checkbox', { name: /ALWAYS/ })
    expect(always).toBeChecked()
    expect(always).toBeDisabled()
  })
})
