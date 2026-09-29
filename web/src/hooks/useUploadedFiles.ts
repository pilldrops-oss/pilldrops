import { useCallback, useState } from 'react'
import { ingestFiles, mergeByPath, stripCommonRoot, type SourceFile } from '../lib/ingestFiles'

/**
 * useUploadedFiles holds the source the user dropped or picked, plus which of
 * those files the browser and editor are pointed at. Uploads accumulate: a
 * second drop adds to the set rather than replacing it.
 */
export function useUploadedFiles() {
  const [files, setFiles] = useState<SourceFile[]>([])
  const [selected, setSelected] = useState<string | null>(null)
  const [opened, setOpened] = useState<string | null>(null)
  const [dragOver, setDragOver] = useState(false)

  const add = useCallback(async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return
    const added = stripCommonRoot(await ingestFiles(fileList)).filter(f => f.path)
    if (!added.length) return
    setFiles(prev => mergeByPath(prev, added))
    setSelected(prev => prev ?? added[0].path)
  }, [])

  const clear = useCallback(() => {
    setFiles([])
    setSelected(null)
    setOpened(null)
  }, [])

  const edit = useCallback((path: string, content: string) => {
    setFiles(prev => prev.map(f => (f.path === path ? { ...f, content } : f)))
  }, [])

  const onDrop = useCallback(async (e: React.DragEvent) => {
    e.preventDefault()
    setDragOver(false)
    if (e.dataTransfer?.files?.length) await add(e.dataTransfer.files)
  }, [add])

  return {
    files, setFiles, selected, setSelected, opened, setOpened,
    dragOver, setDragOver, add, clear, edit, onDrop,
  }
}
