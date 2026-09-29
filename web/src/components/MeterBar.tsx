const CRITICAL_PCT = 90
const WARN_PCT = 70

function level(pct: number): 'ok' | 'warn' | 'crit' {
  if (pct > CRITICAL_PCT) return 'crit'
  if (pct > WARN_PCT) return 'warn'
  return 'ok'
}

export default function MeterBar({
  label,
  fraction,
  value,
}: { label: string; fraction: number; value: string }) {
  const pct = Math.max(0, Math.min(1, fraction)) * 100
  return (
    <div className="meterbar">
      <div className="meterbar-head">
        <span>{label}</span>
        <span className="meterbar-val">{value}</span>
      </div>
      <div
        className="meterbar-track"
        role="meter"
        aria-label={label}
        aria-valuenow={Math.round(pct)}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuetext={value}
      >
        <div className={`meterbar-fill ${level(pct)}`} style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}
