import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import FileBrowser from '../../src/components/FileBrowser'
import { ToastProvider } from '../../src/components/Toast'
import { ConfirmProvider } from '../../src/components/ConfirmModal'
import * as api from '../../src/api'

// Swap the heavy lazy-loaded CodeMirror editor for a textarea.
vi.mock('../../src/components/LazyFileEditor', () => ({
  default: ({ value, onChange }: { value: string; onChange?: (v: string) => void }) => (
    <textarea aria-label="editor" value={value} onChange={e => onChange?.(e.target.value)} />
  ),
}))

vi.mock('../../src/api', () => ({
  fsList: vi.fn(),
  fsRead: vi.fn(),
  fsWrite: vi.fn(),
  fsDownload: vi.fn(),
}))

const entry = (name: string, is_dir = false, size = 10) =>
  ({ name, is_dir, size, modified: '2026-01-01T00:00:00Z' })

function renderBrowser() {
  return render(
    <ToastProvider>
      <ConfirmProvider>
        <FileBrowser id="sb-1" />
      </ConfirmProvider>
    </ToastProvider>,
  )
}

beforeEach(() => {
  vi.mocked(api.fsList).mockReset()
  vi.mocked(api.fsRead).mockReset()
  vi.mocked(api.fsWrite).mockReset()
})

describe('FileBrowser', () => {
  it('lists the sandbox root on mount', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('main.py'), entry('src', true)] })
    renderBrowser()
    expect(await screen.findByText('main.py')).toBeInTheDocument()
    expect(screen.getByText('src')).toBeInTheDocument()
    expect(api.fsList).toHaveBeenCalledWith('sb-1', '.')
  })

  it('loads a directory only on first expand, and reuses it after', async () => {
    vi.mocked(api.fsList).mockImplementation(async (_id: string, path: string) =>
      path === '.' ? { entries: [entry('src', true)] } : { entries: [entry('util.ts')] },
    )
    const user = userEvent.setup()
    renderBrowser()

    await screen.findByText('src')
    expect(api.fsList).toHaveBeenCalledTimes(1)

    await user.click(screen.getByText('src'))
    expect(await screen.findByText('util.ts')).toBeInTheDocument()
    // "./src" would key tree state differently from the "src" the tree renders.
    expect(api.fsList).toHaveBeenCalledWith('sb-1', 'src')

    await user.click(screen.getByText('src'))
    await user.click(screen.getByText('src'))
    await screen.findByText('util.ts')
    expect(api.fsList).toHaveBeenCalledTimes(2)
  })

  it('opens a file into the editor', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('main.py')] })
    vi.mocked(api.fsRead).mockResolvedValue('print("hi")')
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('main.py'))
    await waitFor(() => expect(screen.getByLabelText('editor')).toHaveValue('print("hi")'))
    expect(screen.getByText('Python')).toBeInTheDocument()
  })

  it('enables Save only once the file is edited, then writes it back', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('main.py')] })
    vi.mocked(api.fsRead).mockResolvedValue('one')
    vi.mocked(api.fsWrite).mockResolvedValue(undefined)
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('main.py'))
    const editor = await screen.findByLabelText('editor')

    expect(screen.getByRole('button', { name: /save/i })).toBeDisabled()

    await user.type(editor, '-two')
    expect(screen.getByRole('button', { name: /save/i })).toBeEnabled()

    await user.click(screen.getByRole('button', { name: /save/i }))
    await waitFor(() => expect(api.fsWrite).toHaveBeenCalledWith('sb-1', 'main.py', 'one-two'))
    await waitFor(() => expect(screen.getByRole('button', { name: /save/i })).toBeDisabled())
  })

  it('keeps the edit and reports the failure when saving fails', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('main.py')] })
    vi.mocked(api.fsRead).mockResolvedValue('one')
    vi.mocked(api.fsWrite).mockRejectedValue(new Error('read-only filesystem'))
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('main.py'))
    await user.type(await screen.findByLabelText('editor'), '!')
    await user.click(screen.getByRole('button', { name: /save/i }))

    expect(await screen.findByText(/read-only filesystem/)).toBeInTheDocument()
    expect(screen.getByLabelText('editor')).toHaveValue('one!')
    expect(screen.getByRole('button', { name: /save/i })).toBeEnabled()
  })

  it('will not discard unsaved edits when another file is clicked', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('main.py'), entry('other.py')] })
    vi.mocked(api.fsRead).mockImplementation(async (_id: string, p: string) => `contents of ${p}`)
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('main.py'))
    await user.type(await screen.findByLabelText('editor'), ' EDITED')

    await user.click(screen.getByText('other.py'))

    // Match the dialog's buttons; "unsaved changes" also labels the toolbar's dirty dot.
    expect(await screen.findByRole('button', { name: /keep editing/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /discard/i })).toBeInTheDocument()
    expect(screen.getByLabelText('editor')).toHaveValue('contents of main.py EDITED')

    await user.click(screen.getByRole('button', { name: /keep editing/i }))
    expect(screen.getByLabelText('editor')).toHaveValue('contents of main.py EDITED')
    expect(api.fsRead).not.toHaveBeenCalledWith('sb-1', 'other.py')
  })

  it('switches away once the user confirms discarding', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('main.py'), entry('other.py')] })
    vi.mocked(api.fsRead).mockImplementation(async (_id: string, p: string) => `contents of ${p}`)
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('main.py'))
    await user.type(await screen.findByLabelText('editor'), ' EDITED')
    await user.click(screen.getByText('other.py'))
    await user.click(await screen.findByRole('button', { name: /discard/i }))

    await waitFor(() => expect(screen.getByLabelText('editor')).toHaveValue('contents of other.py'))
  })

  it('switches without asking when there is nothing to lose', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('main.py'), entry('other.py')] })
    vi.mocked(api.fsRead).mockImplementation(async (_id: string, p: string) => `contents of ${p}`)
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('main.py'))
    await screen.findByLabelText('editor')
    await user.click(screen.getByText('other.py'))

    await waitFor(() => expect(screen.getByLabelText('editor')).toHaveValue('contents of other.py'))
    expect(screen.queryByRole('button', { name: /keep editing/i })).toBeNull()
  })

  it('surfaces a directory listing error instead of showing an empty tree', async () => {
    vi.mocked(api.fsList).mockRejectedValue(new Error('sandbox is paused'))
    renderBrowser()
    expect(await screen.findByText(/sandbox is paused/)).toBeInTheDocument()
  })

  // The drop target is the tree pane; fireEvent is the only way to deliver a
  // DataTransfer, which userEvent does not synthesise.
  function dropOn(container: HTMLElement, files: File[]) {
    const tree = container.querySelector('.fx-tree') as HTMLElement
    fireEvent.drop(tree, { dataTransfer: { files } })
    return tree
  }

  const upload = (name: string, body = 'x') => new File([body], name)

  it('uploads dropped files one at a time, in order, at their own paths', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [] })
    vi.mocked(api.fsWrite).mockResolvedValue(undefined)
    const { container } = renderBrowser()
    await waitFor(() => expect(container.querySelector('.fx-tree')).toBeTruthy())

    dropOn(container, [upload('a.txt'), upload('b.txt'), upload('c.txt')])

    await waitFor(() => expect(api.fsWrite).toHaveBeenCalledTimes(3))
    const paths = vi.mocked(api.fsWrite).mock.calls.map(c => c[1])
    expect(paths).toEqual(['a.txt', 'b.txt', 'c.txt'])
    // The File itself is posted, not its text: a 200 MiB drop must not be read
    // into a string first.
    expect(vi.mocked(api.fsWrite).mock.calls[0][2]).toBeInstanceOf(File)
  })

  it('stops at the first failure and leaves the rest un-sent', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [] })
    vi.mocked(api.fsWrite)
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('POST /v1/sandboxes/sb-1/files → 413: file exceeds the per-file limit PD_FILES_MAX_BYTES (1073741824 bytes)'))
      .mockResolvedValue(undefined)
    const { container } = renderBrowser()
    await waitFor(() => expect(container.querySelector('.fx-tree')).toBeTruthy())

    dropOn(container, [upload('a.txt'), upload('big.bin'), upload('c.txt')])

    // The server's own message, not a bare status.
    expect(await screen.findByText(/PD_FILES_MAX_BYTES/)).toBeInTheDocument()
    expect(api.fsWrite).toHaveBeenCalledTimes(2)
    expect(vi.mocked(api.fsWrite).mock.calls.map(c => c[1])).toEqual(['a.txt', 'big.bin'])
  })

  it('refuses a drop of more than 500 files before uploading any of them', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [] })
    vi.mocked(api.fsWrite).mockResolvedValue(undefined)
    const { container } = renderBrowser()
    await waitFor(() => expect(container.querySelector('.fx-tree')).toBeTruthy())

    dropOn(container, Array.from({ length: 501 }, (_, i) => upload(`f${i}.txt`)))

    expect(await screen.findByText(/at most 500/)).toBeInTheDocument()
    expect(api.fsWrite).not.toHaveBeenCalled()
  })

  it('refreshes only the directories an upload actually touched', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [] })
    vi.mocked(api.fsWrite).mockResolvedValue(undefined)
    const { container } = renderBrowser()
    await waitFor(() => expect(api.fsList).toHaveBeenCalledTimes(1))

    const nested = upload('deep.txt')
    Object.defineProperty(nested, 'webkitRelativePath', { value: 'proj/src/deep.txt' })
    dropOn(container, [nested])

    await waitFor(() => expect(api.fsWrite).toHaveBeenCalledTimes(1))
    // The folder picker's relative path is preserved, or a directory upload
    // arrives as a flat pile of leaf names.
    expect(vi.mocked(api.fsWrite).mock.calls[0][1]).toBe('proj/src/deep.txt')
    await waitFor(() => expect(api.fsList).toHaveBeenCalledWith('sb-1', 'proj/src'))
  })

  it('toggles full screen', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [] })
    const user = userEvent.setup()
    const { container } = renderBrowser()

    await waitFor(() => expect(container.querySelector('.fx')).toBeTruthy())
    expect(container.querySelector('.fx-full')).toBeNull()

    await user.click(screen.getByRole('button', { name: 'Full screen' }))
    expect(container.querySelector('.fx-full')).toBeTruthy()

    await user.click(screen.getByRole('button', { name: 'Exit full screen' }))
    expect(container.querySelector('.fx-full')).toBeNull()
  })

  it('refuses to edit a binary file and offers a download', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('a.bin')] })
    vi.mocked(api.fsRead).mockResolvedValue('ELF\u0000\u0001junk')
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('a.bin'))
    expect(await screen.findByText('Binary file — download instead')).toBeInTheDocument()
    expect(screen.queryByLabelText('editor')).toBeNull()
  })

  it('does not even fetch a file too large to edit', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('big.log', false, 50 * 1024 * 1024)] })
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('big.log'))
    expect(await screen.findByText(/Too large to edit \(50\.0 MB\) — download instead/)).toBeInTheDocument()
    expect(api.fsRead).not.toHaveBeenCalled()
    expect(screen.queryByLabelText('editor')).toBeNull()
  })

  it('downloads the open file through an object URL', async () => {
    vi.mocked(api.fsList).mockResolvedValue({ entries: [entry('chart.png', false, 5 * 1024 * 1024)] })
    vi.mocked(api.fsDownload).mockResolvedValue(new Blob(['png']))
    const createURL = vi.fn(() => 'blob:x')
    const revokeURL = vi.fn()
    Object.assign(URL, { createObjectURL: createURL, revokeObjectURL: revokeURL })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      expect(this.download).toBe('chart.png')
      expect(this.href).toBe('blob:x')
    })
    const user = userEvent.setup()
    renderBrowser()

    await user.click(await screen.findByText('chart.png'))
    await user.click(screen.getByRole('button', { name: 'Download chart.png' }))
    await waitFor(() => expect(click).toHaveBeenCalled())
    expect(api.fsDownload).toHaveBeenCalledWith('sb-1', 'chart.png')
    expect(revokeURL).toHaveBeenCalledWith('blob:x')
    click.mockRestore()
  })
})
