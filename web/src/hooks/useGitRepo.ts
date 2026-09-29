import { useCallback, useState } from 'react'
import { parseGitHub, probeRepoManifests } from '../lib/repo'

export type GitProbe = 'idle' | 'checking' | 'ok' | 'none'

export type RepoFile = { path: string; content: string }

/**
 * useGitRepo owns the "clone from GitHub" half of the deploy form: the URL and
 * branch the user typed, whether the repo parses, and the manifests read back
 * from a probe. `enabled` is false while the form is on the upload tab, which
 * is what makes `repo` null there without clearing what the user typed.
 */
export function useGitRepo(enabled: boolean) {
  const [url, setUrlRaw] = useState('')
  const [branch, setBranch] = useState('')
  const [probe, setProbe] = useState<GitProbe>('idle')
  const [files, setFiles] = useState<RepoFile[]>([])

  // Editing the URL invalidates whatever the last probe found, so the hook
  // resets it rather than leaving each caller to remember.
  const setUrl = useCallback((v: string) => {
    setUrlRaw(v)
    setProbe('idle')
    setFiles([])
  }, [])

  const repo = enabled ? parseGitHub(url) : null

  const runProbe = useCallback(async () => {
    const gh = parseGitHub(url)
    if (!gh || probe === 'checking') return
    setProbe('checking')
    setFiles([])
    const found = await probeRepoManifests(fetch, gh, branch)
    setFiles(found)
    setProbe(found.length ? 'ok' : 'none')
  }, [url, branch, probe])

  return { url, setUrl, branch, setBranch, probe, files, repo, runProbe }
}
