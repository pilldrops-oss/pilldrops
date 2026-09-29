import { useState } from 'react'
import { MCP_SERVERS, renderMcpConfig, envVarsFor } from '../lib/mcpCatalog'

export default function McpPicker({
  agentId,
  format,
  value,
  onChange,
  vaultNames,
}: {
  agentId: string
  format: 'json' | 'toml'
  value: string
  onChange: (next: string) => void
  vaultNames?: string[]
}) {
  const [picked, setPicked] = useState<string[]>([])
  const [raw, setRaw] = useState(false)

  function toggle(id: string) {
    const next = picked.includes(id) ? picked.filter(x => x !== id) : [...picked, id]
    setPicked(next)
    onChange(renderMcpConfig(agentId, next))
  }

  const needed = envVarsFor(picked)
  const missing = vaultNames ? needed.filter(n => !vaultNames.includes(n)) : []

  if (raw) {
    return (
      <>
        <textarea
          className="pa-textarea mono"
          rows={format === 'toml' ? 7 : 9}
          spellCheck={false}
          value={value}
          onChange={e => onChange(e.target.value)}
        />
        <p className="lm-help">
          Editing {format.toUpperCase()} directly.{' '}
          <button type="button" className="lm-linkbtn" onClick={() => setRaw(false)}>
            Back to the picker
          </button>{' '}
          — this replaces what you have written.
        </p>
      </>
    )
  }

  return (
    <>
      <div className="mcp-list">
        {MCP_SERVERS.map(s => (
          <label className="mcp-item" key={s.id}>
            <input type="checkbox" checked={picked.includes(s.id)} onChange={() => toggle(s.id)} />
            <span className="mcp-item-txt">
              <b>{s.name}</b>
              <span className="mcp-item-note">{s.note}</span>
            </span>
            {s.env?.length ? <span className="mcp-item-env">{s.env.join(', ')}</span> : null}
          </label>
        ))}
      </div>

      {missing.length > 0 && (
        <p className="lm-hint pa-warn">
          Not in your vault yet: <code>{missing.join(', ')}</code> — add {missing.length === 1 ? 'it' : 'them'} in
          Vault, or {missing.length === 1 ? 'that server' : 'those servers'} will start without credentials.
        </p>
      )}

      {picked.length > 0 && (
        <>
          <div className="field-label mcp-preview-h">
            Writes this {format.toUpperCase()}
          </div>
          <pre className="mcp-preview">{value}</pre>
        </>
      )}

      <p className="lm-help">
        {picked.length === 0 ? 'No servers — the agent runs without any.' : `${picked.length} selected.`}{' '}
        <button
          type="button"
          className="lm-linkbtn"
          onClick={() => {
            onChange(value || renderMcpConfig(agentId, picked))
            setRaw(true)
          }}
        >
          Edit as {format.toUpperCase()} instead
        </button>
      </p>
    </>
  )
}
