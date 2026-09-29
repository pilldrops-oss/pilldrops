import type { TemplateAgentMeta } from '../../types'

export type AgentMode = 'supervised' | 'guided' | 'autonomous'

export function ModeLadder({ agent, value, onChange }: {
  agent?: TemplateAgentMeta
  value: AgentMode
  onChange: (m: AgentMode) => void
}) {
  const gateable = Boolean(agent?.permission_tool)

  return (
    <fieldset className="agent-ladder">
      <legend>How much should it do on its own?</legend>

      <Rung
        mode="supervised" value={value} onChange={onChange}
        label="Supervised — ask me before every action"
        detail="Nothing runs without your approval. Slowest, and the only posture where you see
                every step before it happens."
      />
      <Rung
        mode="guided" value={value} onChange={onChange}
        disabled={!gateable}
        label="Guided — ask me before anything that changes things"
        detail={gateable
          ? 'Reading, searching and running tests go ahead. Writes, installs and commands ' +
            'outside the allowlist wait for you.'
          : undefined}
        disabledReason={
          `This agent's CLI has no approval hook we have verified, so its tool calls cannot be ` +
          `intercepted for review. Offering Guided anyway would mean claiming a review that is ` +
          `not happening.`}
      />
      <Rung
        mode="autonomous" value={value} onChange={onChange}
        label="Autonomous — act without asking"
        detail="Fastest, and appropriate because the sandbox is the boundary: it can change
                anything inside this workstation and nothing outside it."
      />

      {value === 'guided' && gateable && (
        <p className="ws-muted agent-ladder-note">
          If the approval gate cannot start in this workstation, it will run autonomously and
          say so on the Agent tab — rather than reporting reviews that are not happening.
        </p>
      )}
      {value === 'autonomous' && (
        <p className="ws-muted agent-ladder-note">
          You can still stop a turn, and everything it does is on the Timeline.
        </p>
      )}
    </fieldset>
  )
}

function Rung({ mode, value, onChange, label, detail, disabled, disabledReason }: {
  mode: AgentMode
  value: AgentMode
  onChange: (m: AgentMode) => void
  label: string
  detail?: string
  disabled?: boolean
  disabledReason?: string
}) {
  return (
    <label className={`agent-rung${disabled ? ' agent-rung-off' : ''}`}>
      <input
        type="radio"
        name="agent-mode"
        value={mode}
        checked={value === mode}
        disabled={disabled}
        onChange={() => onChange(mode)}
      />
      <span>
        <span className="agent-rung-label">{label}</span>
        <span className="agent-rung-detail">{disabled ? disabledReason : detail}</span>
      </span>
    </label>
  )
}

export function defaultModeFor(agent?: TemplateAgentMeta): AgentMode {
  return agent?.permission_tool ? 'guided' : 'autonomous'
}
