import { useCallback, useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

type ResizeMsg = { type: 'resize'; cols: number; rows: number }

type Props = {
  wsPath: string
  disabledReason?: string
  onReady?: (run: ((cmd: string) => void) | null) => void
}

type Status = 'connecting' | 'open' | 'closed'

const STATUS_LABEL: Record<Status, string> = {
  open: 'connected',
  connecting: 'connecting…',
  closed: 'disconnected',
}

// A preview on its own sandbox domain arrives as an absolute https:// URL, which
// must keep its host; only a relative path is served by the dashboard's origin.
export function socketUrl(wsPath: string, loc: Pick<Location, 'protocol' | 'host'>): string {
  if (/^https?:\/\//.test(wsPath)) return wsPath.replace(/^http/, 'ws')
  return `${loc.protocol === 'https:' ? 'wss' : 'ws'}://${loc.host}${wsPath}`
}

export default function SandboxTerminal({ wsPath, disabledReason, onReady }: Props) {
  const hostRef = useRef<HTMLDivElement | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [gen, setGen] = useState(0)

  const readyRef = useRef(onReady)
  readyRef.current = onReady

  useEffect(() => {
    if (disabledReason || !hostRef.current) return

    const term = new Terminal({
      cursorBlink: true,
      fontFamily: monoFamily(),
      fontSize: 13,
      theme: terminalTheme(),
      scrollback: 5000,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(hostRef.current)
    fit.fit()

    const ws = new WebSocket(socketUrl(wsPath, location))
    ws.binaryType = 'arraybuffer'

    const sendResize = () => {
      if (ws.readyState !== WebSocket.OPEN) return
      const msg: ResizeMsg = { type: 'resize', cols: term.cols, rows: term.rows }
      ws.send(JSON.stringify(msg))
    }

    ws.onopen = () => {
      setStatus('open')
      readyRef.current?.(cmd => {
        if (ws.readyState === WebSocket.OPEN) ws.send(`${cmd  }\n`)
        term.focus()
      })
      sendResize()
      term.focus()
    }
    ws.onmessage = e => {
      term.write(
        typeof e.data === 'string' ? e.data : new Uint8Array(e.data as ArrayBuffer),
      )
    }
    ws.onclose = () => {
      setStatus('closed')
      term.write('\r\n\x1b[38;5;244m[session ended]\x1b[0m\r\n')
    }
    ws.onerror = () => {}

    const onData = term.onData(d => {
      if (ws.readyState === WebSocket.OPEN) ws.send(d)
    })

    const ro = new ResizeObserver(() => {
      try {
        fit.fit()
        sendResize()
      } catch {}
    })
    ro.observe(hostRef.current)

    return () => {
      readyRef.current?.(null)
      ro.disconnect()
      onData.dispose()
      ws.onclose = null
      ws.close()
      term.dispose()
    }
  }, [wsPath, disabledReason, gen])

  const reconnect = useCallback(() => { setStatus('connecting'); setGen(g => g + 1) }, [])

  useEffect(() => {
    if (status !== 'closed') return
    window.addEventListener('online', reconnect)
    return () => window.removeEventListener('online', reconnect)
  }, [status, reconnect])

  if (disabledReason) {
    return <div className="empty">{disabledReason}</div>
  }

  return (
    <div className="term-wrap">
      <div className="term-toolbar">
        <span className={`term-status ${status}`}>
          {STATUS_LABEL[status]}
        </span>
        {status === 'closed' && (
          <button className="link-btn" onClick={reconnect}>
            Reconnect
          </button>
        )}
      </div>
      <div className="term-host" ref={hostRef} />
    </div>
  )
}

function terminalTheme() {
  const pick = cssVar
  return {
    background: pick('--term-bg', '#0b0d10'),
    foreground: pick('--term-fg', '#e6e6e6'),
    cursor: pick('--term-fg', '#e6e6e6'),
  }
}

function monoFamily(): string {
  return cssVar('--mono', 'Menlo, Monaco, "Courier New", monospace')
}

function cssVar(name: string, fallback: string): string {
  const v = getComputedStyle(document.body).getPropertyValue(name).trim()
  return v || fallback
}
