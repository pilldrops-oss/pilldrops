import { lazy, Suspense } from 'react'
import type { FileEditorProps } from './FileEditor'

const FileEditor = lazy(() => import('./FileEditor'))

export default function LazyFileEditor(props: FileEditorProps) {
  return (
    <Suspense fallback={<div className="fx-loading">Loading editor…</div>}>
      <FileEditor {...props} />
    </Suspense>
  )
}
