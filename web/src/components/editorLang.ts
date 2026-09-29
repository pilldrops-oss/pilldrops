import { languageIdFor, type FileLang } from './fileLang'

export type EditorLang = 'python' | 'javascript'

const JS_FAMILY: ReadonlySet<FileLang> = new Set<FileLang>(['javascript', 'json', 'html', 'css'])

export function editorLangFor(path: string): EditorLang {
  return JS_FAMILY.has(languageIdFor(path)) ? 'javascript' : 'python'
}

export function langForTemplate(template: string): EditorLang {
  const t = template.toLowerCase()
  return /node|\bjs\b|javascript|typescript|\bts\b|next|deno|bun/.test(t) ? 'javascript' : 'python'
}
