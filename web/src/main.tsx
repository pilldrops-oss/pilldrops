import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { setSink } from './lib/telemetry'
import './theme.css'

// PostHog runs only when the deployment injects a key into index.html; the
// bundle is embedded in every binary, so a baked-in key would ship self-hosters'
// data. Autocapture and replay stay off: this page holds API keys and env vars.
const posthogKey = window.__SANDBOXD__?.posthogKey
const posthogHost = window.__SANDBOXD__?.posthogHost
if (posthogKey && !posthogKey.startsWith('__')) {
  // Deferred so self-hosted deployments, which never set a key, do not ship it.
  void import('posthog-js').then(({ default: posthog }) => {
    posthog.init(posthogKey, {
      api_host: posthogHost && !posthogHost.startsWith('__') ? posthogHost : 'https://us.i.posthog.com',
      autocapture: false,
      capture_pageview: false,
      capture_pageleave: false,
      disable_session_recording: true,
      person_profiles: 'identified_only',
    })
    setSink((name, props) => posthog.capture(name, props))
  })
} else if (import.meta.env.DEV) {
  // eslint-disable-next-line no-console -- the dev-only telemetry sink is the console
  setSink((name, props) => console.debug('[telemetry]', name, props))
}

// No identity provider: the API key is the credential, exchanged for a session
// cookie at /dashboard/login and re-checked server-side on every /v1 call.
ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
