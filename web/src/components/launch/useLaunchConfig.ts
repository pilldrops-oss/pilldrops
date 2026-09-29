import { useState, useMemo, useEffect } from 'react'

import type { Template, Plan, LimitsInfo } from '../../types'
import { absolutePreviewUrl, getLimits, listSecrets } from '../../api'
import { defaultModeFor, type AgentMode } from '../agent/ModeLadder'

import { type LifetimeKey } from '../../lib/lifetime'

import { type AgentConfig } from '../../lib/launchAgents'
import { type AccessMode, type CustomEnv, type Kind, agentFor, buildEnvList, kindOf } from '../../lib/launchKinds'
import { starterFiles, starterStart } from '../../lib/launchStarters'
import { genSecret } from '../../lib/launchBody'

export interface LaunchConfig {
  tpl: Template | null
  selectTemplate: (t: Template) => void
  clearTemplate: () => void

  cpu: number
  setCpu: (n: number) => void
  mem: number
  setMem: (n: number) => void
  storage: number
  setStorage: (n: number) => void

  start: string
  setStart: (v: string) => void
  port: string
  setPort: (v: string) => void
  timeout: string
  setTimeout: (v: string) => void
  idle: string
  setIdle: (v: string) => void
  lifetime: LifetimeKey
  setLifetime: (v: LifetimeKey) => void
  setup: string
  setSetup: (v: string) => void

  declaredEnv: Record<string, string>
  setDeclaredEnvVar: (name: string, value: string) => void
  customEnv: CustomEnv[]
  setCustomEnv: (rows: CustomEnv[]) => void

  kind: Kind
  agent: AgentConfig | null
  accessMode: AccessMode
  setAccessMode: (m: AccessMode) => void
  agentMode: AgentMode
  setAgentMode: (m: AgentMode) => void
  apiKey: string
  setApiKey: (v: string) => void
  baseUrl: string
  setBaseUrl: (v: string) => void
  model: string
  setModel: (v: string) => void
  mcpJson: string
  setMcpJson: (v: string) => void
  task: string
  setTask: (v: string) => void
  testCmd: string
  setTestCmd: (v: string) => void
  serveReport: boolean
  setServeReport: (v: boolean) => void

  primaryValue: string
  setPrimaryValue: (v: string) => void

  envList: string[]
  previewPort: number
  previewUrl: string
  setupCmds: string[]

  plan: Plan | null

  vaultNames: string[]
  refreshVault: () => void
}

export function useLaunchConfig(): LaunchConfig {
  const [tpl, setTpl] = useState<Template | null>(null)
  const [cpu, setCpu] = useState(1)
  const [mem, setMem] = useState(512)
  const [storage, setStorage] = useState(1024)
  const [start, setStart] = useState('')
  const [port, setPort] = useState('')
  const [timeout, setTimeout] = useState('')
  const [idle, setIdle] = useState('')
  const [lifetime, setLifetime] = useState<LifetimeKey>('idle')
  const [setup, setSetup] = useState('')
  const [declaredEnv, setDeclaredEnv] = useState<Record<string, string>>({})
  const [customEnv, setCustomEnv] = useState<CustomEnv[]>([])

  const [billing, setBilling] = useState<LimitsInfo | null>(null)
  useEffect(() => {
    let alive = true
    getLimits()
      .then(b => {
        if (alive) setBilling(b)
      })
      .catch(() => {})
    return () => {
      alive = false
    }
  }, [])

  const [vaultNames, setVaultNames] = useState<string[]>([])
  const refreshVault = () => {
    listSecrets()
      .then(s => setVaultNames(s.map(x => x.name)))
      .catch(() => {})
  }

  useEffect(refreshVault, [])

  const [accessMode, setAccessMode] = useState<AccessMode>('terminal')
  const [agentModeChoice, setAgentModeChoice] = useState<AgentMode | undefined>(undefined)
  const [apiKey, setApiKey] = useState('')
  const [baseUrl, setBaseUrl] = useState('')
  const [model, setModel] = useState('')
  const [mcpJson, setMcpJson] = useState('')
  const [task, setTask] = useState('')
  const [testCmd, setTestCmd] = useState('')
  const [serveReport, setServeReport] = useState(true)
  const [primaryValue, setPrimaryValue] = useState('')

  function selectTemplate(t: Template) {
    setTpl(t)
    setCpu(t.cpu || 1)
    setMem(t.memory_mb || 512)
    setStorage(t.storage_mb || 1024)
    const dp = t.default_port ?? t.port
    setPort(dp ? String(dp) : '')
    const canSeedStart = !t.has_server && starterFiles(t).length > 0
    setStart(canSeedStart ? (starterStart(t) ?? t.start_suggestion ?? '') : '')
    setSetup((t.setup_suggestion ?? []).join('\n'))
    const seeded: Record<string, string> = {}
    for (const e of t.env ?? []) {
      seeded[e.name] = e.value ?? (e.required && e.secret ? genSecret() : '')
    }
    setDeclaredEnv(seeded)
    setCustomEnv([])

    setAccessMode('terminal')
    setApiKey('')
    setBaseUrl('')
    setModel('')
    setMcpJson(agentFor(t)?.configExample ?? '')
    setTask('')
    setTestCmd(kindOf(t) === 'test' ? (t.start_suggestion ?? 'npx playwright test') : '')
    setServeReport(true)
    setPrimaryValue('')
  }

  function clearTemplate() {
    setTpl(null)
  }

  const envList = useMemo<string[]>(
    () => buildEnvList(tpl?.env ?? [], declaredEnv, customEnv),
    [tpl, declaredEnv, customEnv],
  )

  const kind: Kind = tpl ? kindOf(tpl) : 'web'
  const agent = tpl ? agentFor(tpl) : null

  const previewPort = port ? parseInt(port, 10) : tpl?.default_port ?? tpl?.port ?? 8000
  const previewUrl = absolutePreviewUrl('<id>', previewPort)

  const setupCmds = useMemo(
    () =>
      setup
        .split('\n')
        .map(s => s.trim())
        .filter(Boolean),
    [setup],
  )

  return {
    tpl,
    selectTemplate,
    clearTemplate,
    cpu,
    setCpu,
    mem,
    setMem,
    storage,
    setStorage,
    start,
    setStart,
    port,
    setPort,
    timeout,
    setTimeout,
    idle,
    setIdle,
    lifetime,
    setLifetime,
    setup,
    setSetup,
    declaredEnv,
    setDeclaredEnvVar: (name, value) => setDeclaredEnv(prev => ({ ...prev, [name]: value })),
    customEnv,
    setCustomEnv,
    kind,
    agent,
    accessMode,
    setAccessMode,
    agentMode: agentModeChoice ?? defaultModeFor(tpl?.agent),
    setAgentMode: setAgentModeChoice,
    apiKey,
    setApiKey,
    baseUrl,
    setBaseUrl,
    model,
    setModel,
    mcpJson,
    setMcpJson,
    task,
    setTask,
    testCmd,
    setTestCmd,
    serveReport,
    setServeReport,
    primaryValue,
    setPrimaryValue,
    envList,
    previewPort,
    previewUrl,
    setupCmds,
    plan: billing?.plan ?? null,
    vaultNames,
    refreshVault,
  }
}
