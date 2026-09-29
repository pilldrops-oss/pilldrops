import { useMemo } from 'react'
import CodeMirror, { EditorView } from '@uiw/react-codemirror'
import type { Extension } from '@codemirror/state'
import { StreamLanguage } from '@codemirror/language'
import { python } from '@codemirror/lang-python'
import { javascript } from '@codemirror/lang-javascript'
import { json } from '@codemirror/lang-json'
import { markdown } from '@codemirror/lang-markdown'
import { html } from '@codemirror/lang-html'
import { css } from '@codemirror/lang-css'
import { go } from '@codemirror/legacy-modes/mode/go'
import { rust } from '@codemirror/legacy-modes/mode/rust'
import { yaml } from '@codemirror/legacy-modes/mode/yaml'
import { shell } from '@codemirror/legacy-modes/mode/shell'
import { standardSQL } from '@codemirror/legacy-modes/mode/sql'
import { toml } from '@codemirror/legacy-modes/mode/toml'
import { xml } from '@codemirror/legacy-modes/mode/xml'
import { c } from '@codemirror/legacy-modes/mode/clike'
import { ruby } from '@codemirror/legacy-modes/mode/ruby'
import { dockerFile } from '@codemirror/legacy-modes/mode/dockerfile'
import { githubLight, githubDark } from '@uiw/codemirror-theme-github'
import { useIsDarkTheme } from './useIsDarkTheme'
import type { FileLang } from './fileLang'

const LANG_EXTENSION: Record<FileLang, () => Extension | null> = {
  python: () => python(),
  javascript: () => javascript({ jsx: true, typescript: true }),
  json: () => json(),
  markdown: () => markdown(),
  html: () => html(),
  css: () => css(),
  go: () => StreamLanguage.define(go),
  rust: () => StreamLanguage.define(rust),
  yaml: () => StreamLanguage.define(yaml),
  shell: () => StreamLanguage.define(shell),
  sql: () => StreamLanguage.define(standardSQL),
  toml: () => StreamLanguage.define(toml),
  xml: () => StreamLanguage.define(xml),
  clike: () => StreamLanguage.define(c),
  ruby: () => StreamLanguage.define(ruby),
  php: () => html(),
  dockerfile: () => StreamLanguage.define(dockerFile),
  plain: () => null,
}

export interface FileEditorProps {
  value: string
  language: FileLang
  onChange?: (v: string) => void
  readOnly?: boolean
  onSave?: () => void
}

export default function FileEditor({
  value, language, onChange, readOnly = false, onSave,
}: FileEditorProps) {
  const dark = useIsDarkTheme()

  const extensions = useMemo(() => {
    const exts: Extension[] = [EditorView.lineWrapping]
    const lang = LANG_EXTENSION[language]?.() ?? null
    if (lang) exts.push(lang)
    if (onSave) {
      exts.push(
        EditorView.domEventHandlers({
          keydown: event => {
            if ((event.metaKey || event.ctrlKey) && event.key === 's') {
              event.preventDefault()
              onSave()
              return true
            }
            return false
          },
        }),
      )
    }
    return exts
  }, [language, onSave])

  return (
    <CodeMirror
      className="fx-cm"
      value={value}
      theme={dark ? githubDark : githubLight}
      extensions={extensions}
      editable={!readOnly}
      readOnly={readOnly}
      onChange={onChange}
      height="100%"
      basicSetup={{
        lineNumbers: true,
        foldGutter: true,
        highlightActiveLine: !readOnly,
        highlightActiveLineGutter: !readOnly,
        autocompletion: false,
      }}
    />
  )
}
