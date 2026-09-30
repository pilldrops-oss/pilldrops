import { describe, it, expect } from 'vitest'
import { render, screen, act, fireEvent } from '@testing-library/react'
import { ConfirmProvider, useConfirm } from '../../src/components/ConfirmModal'

type ConfirmFn = ReturnType<typeof useConfirm>

// Captures the provider's confirm function so a test can open a dialog.
function Grab({ into }: { into: { fn?: ConfirmFn } }) {
  into.fn = useConfirm()
  return null
}

async function open(danger: boolean) {
  const ref: { fn?: ConfirmFn } = {}
  render(<ConfirmProvider><Grab into={ref} /></ConfirmProvider>)
  let answer!: Promise<boolean>
  act(() => { answer = ref.fn!({ title: 'Sure?', confirmLabel: 'Do it', danger }) })
  await screen.findByRole('alertdialog')
  return { answer } // wrapped: an async function returning a promise would await it
}

describe('ConfirmModal', () => {
  it('starts a destructive dialog on Cancel, and Enter does not confirm it', async () => {
    const { answer } = await open(true)
    expect(document.activeElement?.textContent).toBe('Cancel')
    fireEvent.keyDown(document, { key: 'Enter' })
    expect(screen.queryByRole('alertdialog')).not.toBeNull()
    fireEvent.click(screen.getByText('Cancel'))
    await expect(answer).resolves.toBe(false)
  })

  it('confirms a non-destructive dialog on Enter', async () => {
    const { answer } = await open(false)
    fireEvent.keyDown(document, { key: 'Enter' })
    await expect(answer).resolves.toBe(true)
  })
})
