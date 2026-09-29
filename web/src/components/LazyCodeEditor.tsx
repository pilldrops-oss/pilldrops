import { lazy, Suspense } from 'react'
import type { CodeEditorProps } from './CodeEditor'

const CodeEditor = lazy(() => import('./CodeEditor'))

export default function LazyCodeEditor(props: CodeEditorProps) {
  const minHeight = props.minHeight ?? 240
  return (
    <Suspense fallback={<div className="editor-loading" style={{ minHeight }}>Loading editor…</div>}>
      <CodeEditor {...props} />
    </Suspense>
  )
}
