import { lazy } from 'react'
import { HashRouter, Routes, Route, Navigate, Link } from 'react-router-dom'
import Shell from './components/Shell'
import { ToastProvider } from './components/Toast'
import { ConfirmProvider } from './components/ConfirmModal'

const Projects = lazy(() => import('./pages/Projects'))
const ProjectDetail = lazy(() => import('./pages/ProjectDetail'))
const Sandboxes = lazy(() => import('./pages/Sandboxes'))
const SandboxDetail = lazy(() => import('./pages/SandboxDetail'))
const Deploy = lazy(() => import('./pages/Deploy'))
const TemplateDetail = lazy(() => import('./pages/TemplateDetail'))
const Usage = lazy(() => import('./pages/Usage'))
const ApiKeys = lazy(() => import('./pages/ApiKeys'))
const Docs = lazy(() => import('./pages/Docs'))
const Templates = lazy(() => import('./pages/Templates'))
const Vault = lazy(() => import('./pages/Vault'))
const Domains = lazy(() => import('./pages/Domains'))
const Volumes = lazy(() => import('./pages/Volumes'))
const Webhooks = lazy(() => import('./pages/Webhooks'))
const Overview = lazy(() => import('./pages/Overview'))
const Sessions = lazy(() => import('./pages/Sessions'))
const Errors = lazy(() => import('./pages/Errors'))
const Feedback = lazy(() => import('./pages/Feedback'))
const Waitlist = lazy(() => import('./pages/Waitlist'))
const Users = lazy(() => import('./pages/Users'))
const UserDetail = lazy(() => import('./pages/UserDetail'))
const AdminSandbox = lazy(() => import('./pages/AdminSandbox'))
const Fleet = lazy(() => import('./pages/Fleet'))
const ClusterSandboxes = lazy(() => import('./pages/ClusterSandboxes'))
const Images = lazy(() => import('./pages/Images'))
const AddNode = lazy(() => import('./pages/AddNode'))
const System = lazy(() => import('./pages/System'))

// mode is injected into index.html at boot: 'admin' when served by the orchestrator.
const IS_ADMIN = (window.__SANDBOXD__?.mode ?? 'user') === 'admin'

function NotFound() {
  return (
    <div className="wrap">
      <Link className="back-link" to="/">← Back</Link>
      <div className="detail-loading">Page not found.</div>
    </div>
  )
}

export default function App() {
  const app = (
    <HashRouter>
      <Routes>
        <Route path="/" element={<Shell />}>
          <Route index element={<Navigate to={IS_ADMIN ? '/overview' : '/sandboxes'} replace />} />

          <Route path="sandboxes" element={<Sandboxes />} />
          <Route path="projects" element={<Projects />} />
          <Route path="projects/:id" element={<ProjectDetail />} />
          <Route path="sandbox/:id" element={<SandboxDetail />} />
          <Route path="deploy" element={<Deploy />} />
          <Route path="domains" element={<Domains />} />
          <Route path="volumes" element={<Volumes />} />
          <Route path="webhooks" element={<Webhooks />} />
          <Route path="t/:id" element={<TemplateDetail />} />
          <Route path="templates" element={<Templates />} />
          <Route path="vault" element={<Vault />} />
          <Route path="usage" element={<Usage />} />
          {/* Old bookmarked URLs, kept as redirects. */}
          <Route path="history" element={<Navigate to="/sandboxes?tab=past" replace />} />
          <Route path="logs" element={<Navigate to="/sandboxes" replace />} />
          <Route path="keys" element={<ApiKeys />} />
          <Route path="docs" element={<Docs />} />

          {/* Admin routes. The nav hides them for customers; the server enforces access. */}
          <Route path="fleet" element={<Fleet />} />
          <Route path="cluster-sandboxes" element={<ClusterSandboxes />} />
          <Route path="images" element={<Images />} />
          <Route path="add-node" element={<AddNode />} />
          <Route path="system" element={<System />} />
          <Route path="overview" element={<Overview />} />
          <Route path="sessions" element={<Sessions />} />
          <Route path="errors" element={<Errors />} />
          <Route path="feedback" element={<Feedback />} />
          <Route path="waitlist" element={<Waitlist />} />
          <Route path="users" element={<Users />} />
          <Route path="users/:ownerId" element={<UserDetail />} />
          <Route path="sandboxes/:id" element={<AdminSandbox />} />
          <Route path="*" element={<NotFound />} />
        </Route>
      </Routes>
    </HashRouter>
  )
  // No identity gate: holding the API key is the authorisation, checked server-side per call.
  const gated = app

  return (
    <ToastProvider>
      <ConfirmProvider>{gated}</ConfirmProvider>
    </ToastProvider>
  )
}
