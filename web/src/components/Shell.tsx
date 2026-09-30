import { useEffect, useState, useCallback, Suspense } from 'react'
import { Outlet, NavLink, useNavigate, useLocation } from 'react-router-dom'
import ErrorBoundary from './ErrorBoundary'
import { RowsSkeleton } from './Skeleton'
import { labelFor } from '../lib/navLabels'
import { clearThread } from '../lib/threadStore'

const FUNCTIONS_ENABLED = false

const MODE = window.__SANDBOXD__?.mode ?? 'user'
const IS_ADMIN = MODE === 'admin'

const ENV_LABEL = IS_ADMIN ? 'cluster' : (window.__SANDBOXD__?.runtime ?? 'local')

function getInitialTheme(): boolean {
  try {
    return localStorage.getItem('sandboxd-theme') === 'dark'
  } catch {
    return false
  }
}

export default function Shell() {
  const [dark, setDark] = useState(getInitialTheme)
  const [mobileNav, setMobileNav] = useState(false)
  const navigate = useNavigate()
  const loc = useLocation()

  useEffect(() => {
    document.body.classList.toggle('dark', dark)
    try { localStorage.setItem('sandboxd-theme', dark ? 'dark' : 'light') } catch {}
  }, [dark])

  const toggleTheme = useCallback(() => setDark(d => !d), [])

  return (
    <div className={`app${mobileNav ? ' nav-open' : ''}`}>
      {mobileNav && <div className="side-backdrop" onClick={() => setMobileNav(false)} />}
      <aside
        className={`side${mobileNav ? ' open' : ''}`}
        onClick={e => { if ((e.target as HTMLElement).closest('a')) setMobileNav(false) }}
      >
        <div className="ws">
          <div className="mark" />
          <div className="nm">
            {IS_ADMIN ? 'pilldrops admin' : 'pilldrops'}
            <span className="beta">Beta</span>
            <small>{IS_ADMIN ? 'control plane · cluster' : 'self-hosted sandboxes'}</small>
          </div>
        </div>

        {IS_ADMIN ? <AdminNav /> : <UserNav />}

        <div className="foot">
          <div className="rt">
            <span className="d" />
            runtime · {window.__SANDBOXD__?.runtime ?? 'process'} · v1.0
          </div>
        </div>
      </aside>

      <main className="main">
        <TopBar
          dark={dark}
          onToggle={toggleTheme}
          onDeploy={() => navigate(IS_ADMIN ? '/add-node' : '/deploy')}
          onOpenNav={() => setMobileNav(true)}
            onNew={() => { clearThread(); navigate(IS_ADMIN ? '/add-node' : '/deploy'); }}
        />
        <ErrorBoundary resetKey={loc.pathname}>
          <Suspense fallback={<div className="wrap"><RowsSkeleton rows={6} /></div>}>
            <Outlet />
          </Suspense>
        </ErrorBoundary>
      </main>
    </div>
  )
}

function UserNav() {
  return (
    <>
      <nav className="nav">
        <NavLink to="/sandboxes" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <rect x="3" y="3" width="7" height="7" rx="1.5" />
            <rect x="14" y="3" width="7" height="7" rx="1.5" />
            <rect x="3" y="14" width="7" height="7" rx="1.5" />
            <rect x="14" y="14" width="7" height="7" rx="1.5" />
          </svg>
          Sandboxes
        </NavLink>
        {FUNCTIONS_ENABLED && (
        <NavLink to="/functions" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M4 7h16M4 12h16M4 17h16" />
            <circle cx="8" cy="7" r="1.4" />
            <circle cx="8" cy="12" r="1.4" />
            <circle cx="8" cy="17" r="1.4" />
          </svg>
          Functions
        </NavLink>
        )}
        <NavLink to="/vault" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <rect x="4" y="10" width="16" height="10" rx="2" />
            <path d="M8 10V7a4 4 0 0 1 8 0v3M12 14v3" />
          </svg>
          Vault
        </NavLink>
        <NavLink to="/domains" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <circle cx="12" cy="12" r="9" />
            <path d="M3 12h18M12 3c3 3.5 3 14 0 18M12 3c-3 3.5-3 14 0 18" />
          </svg>
          Domains
        </NavLink>
        <NavLink to="/usage" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M4 19V9M10 19V4M16 19v-7M22 19H2" />
          </svg>
          Usage
        </NavLink>
      </nav>

      <div className="sect">Account</div>

      <nav className="nav">
        <NavLink to="/templates" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <rect x="3" y="4" width="18" height="16" rx="2" />
            <path d="M3 9h18M8 9v11" />
          </svg>
          Templates
        </NavLink>
        <NavLink to="/keys" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <circle cx="8" cy="9" r="4" />
            <path d="M12 12l8 8m-3 0h3v-3" />
          </svg>
          API Keys
        </NavLink>
        <NavLink to="/docs" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M4 5h16v14H4zM8 9h8M8 13h5" />
          </svg>
          Docs
        </NavLink>
      </nav>
    </>
  )
}

function AdminNav() {
  return (
    <>
      <div className="sect">Analytics</div>
      <nav className="nav">
        <NavLink to="/overview" end className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M4 19V9M10 19V4M16 19v-7M22 19H2" />
          </svg>
          Overview
        </NavLink>
        <NavLink to="/sessions" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M12 8v5l3 2M3 12a9 9 0 1 0 3-6.7L3 8" />
          </svg>
          Sessions
        </NavLink>
        <NavLink to="/errors" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M12 3l10 18H2L12 3z" />
            <path d="M12 10v4M12 18h.01" />
          </svg>
          Errors
        </NavLink>
        <NavLink to="/feedback" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
          </svg>
          Feedback
        </NavLink>
        <NavLink to="/waitlist" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M4 4h16v16H4z" />
            <path d="M4 6l8 7 8-7" />
          </svg>
          Waitlist
        </NavLink>
        <NavLink to="/users" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <circle cx="9" cy="8" r="3.5" />
            <path d="M2 20c0-4 3.1-7 7-7h4c3.9 0 7 3 7 7" />
            <path d="M17 6v6M14 9h6" />
          </svg>
          Users
        </NavLink>
      </nav>

      <div className="sect">Fleet</div>
      <nav className="nav">
        <NavLink to="/fleet" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <rect x="3" y="4" width="18" height="5" rx="1.5" />
            <rect x="3" y="11" width="18" height="5" rx="1.5" />
            <circle cx="7" cy="6.5" r="1" />
            <circle cx="7" cy="13.5" r="1" />
          </svg>
          Fleet
        </NavLink>
        <NavLink to="/cluster-sandboxes" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <rect x="3" y="3" width="7" height="7" rx="1.5" />
            <rect x="14" y="3" width="7" height="7" rx="1.5" />
            <rect x="3" y="14" width="7" height="7" rx="1.5" />
            <rect x="14" y="14" width="7" height="7" rx="1.5" />
          </svg>
          Cluster Sandboxes
        </NavLink>
        <NavLink to="/images" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <rect x="3" y="4" width="18" height="16" rx="2" />
            <circle cx="8.5" cy="9" r="1.8" />
            <path d="M21 16l-5-5-9 9" />
          </svg>
          Images
        </NavLink>
      </nav>

      <div className="sect">System</div>
      <nav className="nav">
        <NavLink to="/add-node" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <rect x="3" y="4" width="18" height="5" rx="1.5" />
            <path d="M12 13v6M9 16h6" />
          </svg>
          Add Node
        </NavLink>
        <NavLink to="/system" className={({ isActive }) => isActive ? 'on' : ''}>
          <svg viewBox="0 0 24 24">
            <path d="M4 19V9M10 19V4M16 19v-7M22 19H2" />
          </svg>
          System / Scale
        </NavLink>
      </nav>
    </>
  )
}

interface TopBarProps {
  dark: boolean
  onToggle: () => void
  onDeploy: () => void
  onOpenNav: () => void
  onNew: () => void
}

function TopBar({ onToggle, onDeploy, onOpenNav, onNew }: TopBarProps) {
  return (
    <div className="top">
      <div className="top-left">
        <button className="hamburger" onClick={onOpenNav} aria-label="Open navigation menu">
          <svg viewBox="0 0 24 24"><path d="M4 7h16M4 12h16M4 17h16" /></svg>
        </button>
        <Breadcrumb />
      </div>
      <div className="tr">
        <button className="toggle" onClick={onToggle} title="Toggle theme" aria-label="Toggle dark mode">
          ◐
        </button>
        <span className="env" title="Where your sandboxes run">
          <span className="d" />
          {ENV_LABEL}
        </span>
        {IS_ADMIN ? (
          <button className="btn primary" onClick={onDeploy}>
            <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.2">
              <path d="M12 5v14M5 12h14" />
            </svg>
            Add Node
          </button>
        ) : (
          <button className="btn" onClick={onNew}>
            ＋ New
          </button>
        )}
      </div>
    </div>
  )
}

function Breadcrumb() {
  const location = useLocation()
  const parts = location.pathname.replace(/^\//, '').split('/')
  const first = parts[0] ?? ''

  const label = labelFor(first, IS_ADMIN)
  const sub = (first === 'sandbox' || first === 'fn') && parts[1] ? parts[1] : null

  return (
    <div className="bc">
      {IS_ADMIN ? 'pilldrops admin' : 'pilldrops'} <span className="sep">/</span> <b>{label}</b>
      {sub && (
        <>
          <span className="sep">/</span>
          <span style={{ fontFamily: 'var(--mono)', fontSize: 12, color: 'var(--sub)' }}>{sub}</span>
        </>
      )}
    </div>
  )
}
