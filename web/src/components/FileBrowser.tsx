import { useState, useEffect, useCallback, useRef } from 'react'
import { fsDownload, fsList, fsRead, fsWrite } from '../api'
import type { FilesystemEntry } from '../types'
import { joinPath, baseName, ROOT } from '../lib/filePath'
import { languageIdFor, LANGUAGE_LABEL } from './fileLang'
import LazyFileEditor from './LazyFileEditor'
import { useToast } from './Toast'
import { useConfirm } from './ConfirmModal'
import { errText } from '../lib/errText'

// One drop or pick at a time. Each file is its own request, so a dropped
// node_modules would be tens of thousands of them; refuse before starting
// rather than a minute in.
const MAX_UPLOAD_FILES = 500

// Past this the editor stalls and a save would round-trip megabytes; download instead.
const MAX_EDIT_BYTES = 2 * 1024 * 1024

// A NUL in the first chunk is the same binary test git and grep use.
function looksBinary(text: string): boolean {
  return text.slice(0, 8000).includes('\u0000')
}

function formatFileSize(bytes: number): string {
  if (bytes === 0) return '0 B'
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

interface TreeState {
  children: Record<string, FilesystemEntry[]>
  loading: Record<string, boolean>
  errors: Record<string, string>
}

export default function FileBrowser({ id }: { id: string }) {
  const toast = useToast()
  const confirm = useConfirm()

  const [tree, setTree] = useState<TreeState>({ children: {}, loading: {}, errors: {} })
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set([ROOT]))

  const [openPath, setOpenPath] = useState<string | null>(null)
  const [saved, setSaved] = useState('')
  const [draft, setDraft] = useState('')
  const [loadingFile, setLoadingFile] = useState(false)
  const [fileError, setFileError] = useState<string | null>(null)
  // Why the open file is not shown in the editor, or '' when it is.
  const [noEdit, setNoEdit] = useState('')
  const [saving, setSaving] = useState(false)

  const [fullscreen, setFullscreen] = useState(false)
  const [creating, setCreating] = useState(false)
  const [newPath, setNewPath] = useState('')

  const [dragOver, setDragOver] = useState(false)
  // { done, total } while an upload runs, null otherwise.
  const [uploading, setUploading] = useState<{ done: number; total: number } | null>(null)

  const dirty = openPath !== null && draft !== saved

  const loadDir = useCallback(async (path: string) => {
    setTree(t => ({ ...t, loading: { ...t.loading, [path]: true }, errors: { ...t.errors, [path]: '' } }))
    try {
      const resp = await fsList(id, path)
      if (resp.entries != null && !Array.isArray(resp.entries)) {
        throw new Error('the sandbox answered with something that isn’t a file listing — it may still be booting')
      }
      const entries = [...(resp.entries ?? [])].sort((a, b) => {
        if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1
        return a.name.localeCompare(b.name)
      })
      setTree(t => ({
        children: { ...t.children, [path]: entries },
        loading: { ...t.loading, [path]: false },
        errors: { ...t.errors, [path]: '' },
      }))
    } catch (e) {
      setTree(t => ({
        ...t,
        loading: { ...t.loading, [path]: false },
        errors: { ...t.errors, [path]: errText(e) },
      }))
    }
  }, [id])

  useEffect(() => { loadDir(ROOT) }, [loadDir])

  const toggleDir = useCallback((path: string) => {
    const willExpand = !expanded.has(path)
    setExpanded(prev => {
      const next = new Set(prev)
      if (next.has(path)) next.delete(path)
      else next.add(path)
      return next
    })
    if (willExpand && !(path in tree.children) && !tree.loading[path]) loadDir(path)
  }, [expanded, tree, loadDir])

  const openFile = useCallback(async (path: string, size = 0) => {
    if (path === openPath) return
    if (openPath !== null && draft !== saved) {
      const ok = await confirm({
        title: 'Unsaved changes',
        body: (
          <>
            You have unsaved changes to <code>{baseName(openPath)}</code>. Opening another
            file will discard them.
          </>
        ),
        confirmLabel: 'Discard changes',
        cancelLabel: 'Keep editing',
        danger: true,
      })
      if (!ok) return
    }
    setOpenPath(path)
    setFileError(null)
    setSaved('')
    setDraft('')
    const tooBig = size > MAX_EDIT_BYTES
    setNoEdit(tooBig ? `Too large to edit (${formatFileSize(size)})` : '')
    if (tooBig) return
    setLoadingFile(true)
    try {
      const text = await fsRead(id, path)
      if (looksBinary(text)) setNoEdit('Binary file')
      else {
        setSaved(text)
        setDraft(text)
      }
    } catch (e) {
      setFileError(errText(e))
    }
    setLoadingFile(false)
  }, [id, openPath, draft, saved, confirm])

  const download = useCallback(async () => {
    if (!openPath) return
    try {
      const url = URL.createObjectURL(await fsDownload(id, openPath))
      const a = document.createElement('a')
      a.href = url
      a.download = baseName(openPath)
      document.body.appendChild(a)
      a.click()
      a.remove()
      URL.revokeObjectURL(url)
    } catch (e) {
      toast.error(`Could not download ${baseName(openPath)}: ${errText(e)}`)
    }
  }, [id, openPath, toast])

  const saveRef = useRef<() => void>(() => {})

  const save = useCallback(async () => {
    if (!openPath || saving) return
    setSaving(true)
    const body = draft
    try {
      await fsWrite(id, openPath, body)
      setSaved(body)
      toast.success(`Saved ${baseName(openPath)}`)
    } catch (e) {
      toast.error(`Could not save ${baseName(openPath)}: ${errText(e)}`)
    }
    setSaving(false)
  }, [id, openPath, draft, saving, toast])

  useEffect(() => { saveRef.current = save }, [save])
  const onSave = useCallback(() => saveRef.current(), [])

  const createFile = useCallback(async () => {
    const path = newPath.trim()
    if (!path) return
    try {
      await fsWrite(id, path, '')
      setCreating(false)
      setNewPath('')
      toast.success(`Created ${baseName(path)}`)
      const parent = path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : ROOT
      await loadDir(parent)
      openFile(path)
    } catch (e) {
      toast.error(`Could not create ${path}: ${errText(e)}`)
    }
  }, [id, newPath, loadDir, openFile, toast])

  const upload = useCallback(async (list: FileList | null) => {
    const files = Array.from(list ?? [])
    if (!files.length || uploading) return
    if (files.length > MAX_UPLOAD_FILES) {
      toast.error(
        `That is ${files.length} files; this uploads at most ${MAX_UPLOAD_FILES} at a time. ` +
        'Zip the folder and upload that, or pick fewer files.',
      )
      return
    }

    // Sequential, not parallel: each POST streams a whole file body into the
    // sandbox, and N of those at once is N file-sized buffers on the node.
    setUploading({ done: 0, total: files.length })
    const touched = new Set<string>()
    for (let i = 0; i < files.length; i++) {
      const f = files[i]
      // webkitRelativePath is set by the folder picker, so a directory upload
      // lands as a directory rather than as a flat pile of leaf names.
      const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath || f.name
      try {
        await fsWrite(id, rel, f)
      } catch (e) {
        // Stop on the first failure with the server's own message: carrying on
        // past a 413 or a full disk would just repeat it N more times.
        setUploading(null)
        toast.error(`Upload stopped at ${baseName(rel)} (${i + 1} of ${files.length}): ${errText(e)}`)
        for (const d of touched) await loadDir(d)
        return
      }
      touched.add(rel.includes('/') ? rel.slice(0, rel.lastIndexOf('/')) : ROOT)
      setUploading({ done: i + 1, total: files.length })
    }
    setUploading(null)
    toast.success(`Uploaded ${files.length} file${files.length === 1 ? '' : 's'}`)
    // Only the directories that actually received something.
    for (const d of touched) await loadDir(d)
  }, [id, uploading, toast, loadDir])

  useEffect(() => {
    if (!fullscreen) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setFullscreen(false) }
    window.addEventListener('keydown', onKey)
    const prev = document.body.style.overflow
    document.body.style.overflow = 'hidden'
    return () => {
      window.removeEventListener('keydown', onKey)
      document.body.style.overflow = prev
    }
  }, [fullscreen])

  useEffect(() => {
    if (!dirty) return
    const warn = (e: BeforeUnloadEvent) => { e.preventDefault() }
    window.addEventListener('beforeunload', warn)
    return () => window.removeEventListener('beforeunload', warn)
  }, [dirty])

  function renderDir(path: string, depth: number): React.ReactNode {
    const entries = tree.children[path]
    const isLoading = tree.loading[path]
    const error = tree.errors[path]

    if (isLoading && !entries) {
      return <div className="fx-tree-note" style={{ paddingLeft: 12 + depth * 13 }}>Loading…</div>
    }
    if (error) {
      return <div className="fx-tree-note fx-tree-err" style={{ paddingLeft: 12 + depth * 13 }}>{error}</div>
    }
    if (entries && entries.length === 0) {
      return <div className="fx-tree-note" style={{ paddingLeft: 12 + depth * 13 }}>empty</div>
    }
    return entries?.map(entry => {
      const childPath = joinPath(path, entry.name)
      const isOpen = expanded.has(childPath)
      if (entry.is_dir) {
        return (
          <div key={childPath}>
            <button
              type="button"
              className="fx-node fx-dir"
              style={{ paddingLeft: 6 + depth * 13 }}
              onClick={() => toggleDir(childPath)}
              aria-expanded={isOpen}
              title={childPath}
            >
              <span className={`fx-caret${isOpen ? ' open' : ''}`} aria-hidden>▸</span>
              <span className="fx-node-name">{entry.name}</span>
            </button>
            {isOpen && renderDir(childPath, depth + 1)}
          </div>
        )
      }
      return (
        <button
          key={childPath}
          type="button"
          className={`fx-node fx-file${openPath === childPath ? ' active' : ''}`}
          style={{ paddingLeft: 6 + depth * 13 }}
          onClick={() => openFile(childPath, entry.size)}
          title={childPath}
        >
          <span className="fx-caret" aria-hidden />
          <span className="fx-node-name">{entry.name}</span>
          <span className="fx-node-size">{formatFileSize(entry.size)}</span>
        </button>
      )
    })
  }

  // Reset the input's value so picking the same file twice still fires change.
  const onPick = (e: React.ChangeEvent<HTMLInputElement>) => {
    void upload(e.target.files)
    e.target.value = ''
  }

  const lang = openPath ? languageIdFor(openPath) : 'plain'

  return (
    <div className={`fx${fullscreen ? ' fx-full' : ''}`}>
      <div className="fx-side">
        <div className="fx-side-head">
          <span className="fx-side-title">Files</span>
          <button
            type="button"
            className="fx-icon-btn"
            onClick={() => { setCreating(c => !c); setNewPath('') }}
            title="New file"
            aria-label="New file"
          >
            +
          </button>
          <label className="fx-icon-btn fx-upload-btn" title="Upload files" aria-label="Upload files">
            ⤒
            <input type="file" multiple hidden onChange={onPick} />
          </label>
          <label className="fx-icon-btn fx-upload-btn" title="Upload a folder" aria-label="Upload a folder">
            ⊞
            {/* @ts-expect-error non-standard but widely supported directory picker */}
            <input type="file" hidden webkitdirectory="" directory="" multiple onChange={onPick} />
          </label>
          <button
            type="button"
            className="fx-icon-btn"
            onClick={() => loadDir(ROOT)}
            title="Refresh"
            aria-label="Refresh file tree"
          >
            ⟳
          </button>
        </div>

        {uploading && (
          <div className="fx-upload-pill" role="status">
            Uploading {uploading.done}/{uploading.total}…
          </div>
        )}

        {creating && (
          <div className="fx-new">
            <input
              className="fx-new-input"
              value={newPath}
              autoFocus
              placeholder="src/hello.py"
              onChange={e => setNewPath(e.target.value)}
              onKeyDown={e => {
                if (e.key === 'Enter') createFile()
                if (e.key === 'Escape') { setCreating(false); setNewPath('') }
              }}
              aria-label="New file path"
            />
            <button type="button" className="btn primary fx-new-go" onClick={createFile} disabled={!newPath.trim()}>
              Create
            </button>
          </div>
        )}

        <div
          className={`fx-tree${dragOver ? ' fx-drop' : ''}`}
          onDragOver={e => { e.preventDefault(); setDragOver(true) }}
          onDragLeave={() => setDragOver(false)}
          onDrop={e => {
            e.preventDefault()
            setDragOver(false)
            void upload(e.dataTransfer?.files ?? null)
          }}
        >
          {renderDir(ROOT, 0)}
        </div>
      </div>

      <div className="fx-main">
        <div className="fx-bar">
          {openPath ? (
            <>
              <span className="fx-bar-path" title={openPath}>
                {openPath}
                {dirty && <span className="fx-dirty" title="Unsaved changes" aria-label="Unsaved changes">●</span>}
              </span>
              <span className="fx-bar-lang">{noEdit ? '—' : LANGUAGE_LABEL[lang]}</span>
              <button type="button" className="btn ghost" onClick={download} title="Download this file">
                Download
              </button>
              <button
                type="button"
                className="btn primary fx-save"
                onClick={save}
                disabled={!dirty || saving}
                title="Save (⌘S)"
              >
                {saving ? 'Saving…' : 'Save'}
              </button>
            </>
          ) : (
            <span className="fx-bar-path fx-bar-empty">No file open</span>
          )}
          <button
            type="button"
            className="fx-icon-btn fx-expand"
            onClick={() => setFullscreen(f => !f)}
            title={fullscreen ? 'Exit full screen (Esc)' : 'Full screen'}
            aria-label={fullscreen ? 'Exit full screen' : 'Full screen'}
          >
            {fullscreen ? '⤡' : '⤢'}
          </button>
        </div>

        <div className="fx-editor">
          {!openPath && (
            <div className="fx-empty">
              <svg viewBox="0 0 24 24" aria-hidden>
                <path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z" />
                <path d="M13 2v7h7" />
              </svg>
              <span>Select a file from the tree to open it</span>
            </div>
          )}
          {openPath && loadingFile && <div className="fx-loading">Loading {baseName(openPath)}…</div>}
          {openPath && fileError && <div className="fx-file-err">{fileError}</div>}
          {openPath && noEdit && (
            <div className="fx-empty">
              <span>{noEdit} — download instead</span>
              <button type="button" className="btn" onClick={download}>Download {baseName(openPath)}</button>
            </div>
          )}
          {openPath && !loadingFile && !fileError && !noEdit && (
            <LazyFileEditor value={draft} language={lang} onChange={setDraft} onSave={onSave} />
          )}
        </div>
      </div>
    </div>
  )
}
