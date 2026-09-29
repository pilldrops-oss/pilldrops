import { useMemo } from 'react'
import CodeMirror, { EditorView } from '@uiw/react-codemirror'
import { python } from '@codemirror/lang-python'
import { javascript } from '@codemirror/lang-javascript'
import { githubLight } from '@uiw/codemirror-theme-github'
import type { EditorLang } from './editorLang'

const LANG_LABEL: Record<EditorLang, string> = { python: 'Python', javascript: 'JavaScript' }

export interface CodeEditorProps {
  value: string
  onChange?: (v: string) => void
  language: EditorLang
  onLanguageChange?: (l: EditorLang) => void
  filename?: string
  readOnly?: boolean
  minHeight?: number
  maxHeight?: number
  actions?: React.ReactNode
}

export default function CodeEditor({
  value, onChange, language, onLanguageChange, filename,
  readOnly = false, minHeight = 240, maxHeight = 460, actions,
}: CodeEditorProps) {
  const extensions = useMemo(
    () => [
      language === 'javascript' ? javascript() : python(),
      EditorView.lineWrapping,
    ],
    [language],
  )

  return (
    <div className="editor">
      <div className="editor-bar">
        <div className="editor-dots" aria-hidden>
          <span /><span /><span />
        </div>
        {filename && <span className="editor-file">{filename}</span>}
        <div className="editor-bar-right">
          {onLanguageChange ? (
            <select
              className="editor-lang"
              value={language}
              onChange={e => onLanguageChange(e.target.value as EditorLang)}
              aria-label="Editor language"
            >
              <option value="python">Python</option>
              <option value="javascript">JavaScript</option>
            </select>
          ) : (
            <span className="editor-lang-label">{LANG_LABEL[language]}</span>
          )}
          {actions}
        </div>
      </div>
      <CodeMirror
        value={value}
        theme={githubLight}
        extensions={extensions}
        editable={!readOnly}
        readOnly={readOnly}
        onChange={onChange}
        minHeight={`${minHeight}px`}
        maxHeight={`${maxHeight}px`}
        basicSetup={{
          lineNumbers: true,
          foldGutter: false,
          highlightActiveLine: !readOnly,
          highlightActiveLineGutter: !readOnly,
          autocompletion: false,
        }}
      />
    </div>
  )
}
