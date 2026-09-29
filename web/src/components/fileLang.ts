
export type FileLang =
  | 'python' | 'javascript' | 'json' | 'markdown' | 'html' | 'css' | 'yaml'
  | 'go' | 'rust' | 'shell' | 'sql' | 'toml' | 'xml' | 'clike' | 'ruby' | 'php'
  | 'dockerfile' | 'plain'

export const LANGUAGE_LABEL: Record<FileLang, string> = {
  python: 'Python',
  javascript: 'JavaScript',
  json: 'JSON',
  markdown: 'Markdown',
  html: 'HTML',
  css: 'CSS',
  yaml: 'YAML',
  go: 'Go',
  rust: 'Rust',
  shell: 'Shell',
  sql: 'SQL',
  toml: 'TOML',
  xml: 'XML',
  clike: 'C-like',
  ruby: 'Ruby',
  php: 'PHP',
  dockerfile: 'Dockerfile',
  plain: 'Plain text',
}

const BY_EXTENSION: Record<string, FileLang> = {
  py: 'python', pyw: 'python',
  js: 'javascript', jsx: 'javascript', mjs: 'javascript', cjs: 'javascript',
  ts: 'javascript', tsx: 'javascript',
  json: 'json', jsonc: 'json',
  md: 'markdown', markdown: 'markdown',
  html: 'html', htm: 'html',
  css: 'css', scss: 'css', less: 'css',
  yml: 'yaml', yaml: 'yaml',
  go: 'go',
  rs: 'rust',
  sh: 'shell', bash: 'shell', zsh: 'shell',
  sql: 'sql',
  toml: 'toml',
  xml: 'xml', svg: 'xml',
  c: 'clike', h: 'clike', cpp: 'clike', cc: 'clike', hpp: 'clike', java: 'clike',
  rb: 'ruby',
  php: 'php',
}

const BY_BASENAME: Record<string, FileLang> = {
  dockerfile: 'dockerfile',
}

export function languageIdFor(path: string): FileLang {
  const base = (path.split('/').pop() ?? '').toLowerCase()
  if (!base) return 'plain'

  const leading = base.split('.')[0]
  if (BY_BASENAME[leading]) return BY_BASENAME[leading]

  const dot = base.lastIndexOf('.')
  if (dot <= 0) return 'plain'
  return BY_EXTENSION[base.slice(dot + 1)] ?? 'plain'
}
