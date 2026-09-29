import type { CSSProperties } from 'react'
import { serverOrigin } from '../api'

const linkCard: CSSProperties = {
  display: 'block', padding: '18px 20px', border: '1px solid var(--hair, rgba(128,128,128,.25))',
  borderRadius: 10, textDecoration: 'none', color: 'inherit',
}

export default function Docs() {
  return (
    <div className="wrap">
      <div className="h1row">
        <div>
          <h1 className="h1">Docs</h1>
          <p>SDK guides, the runnable API reference, and use-case recipes</p>
        </div>
      </div>

      <div className="docs-content">
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: 14, margin: '18px 0 8px' }}>
          <a href="/sdk" target="_blank" rel="noreferrer" style={linkCard}>
            <h3 style={{ margin: '0 0 6px' }}>SDK documentation →</h3>
            <p style={{ margin: 0 }}>
              Python &amp; JavaScript SDKs + CLI: install, auth, every <code>Sandbox</code> method,
              preview URLs, custom images, common workflows.
            </p>
          </a>
          <a href="/docs" target="_blank" rel="noreferrer" style={linkCard}>
            <h3 style={{ margin: '0 0 6px' }}>API reference →</h3>
            <p style={{ margin: 0 }}>
              Every endpoint, runnable in the browser — paste a key and hit the live API from the page.
            </p>
          </a>
          <a href="/guides" target="_blank" rel="noreferrer" style={linkCard}>
            <h3 style={{ margin: '0 0 6px' }}>Guides →</h3>
            <p style={{ margin: 0 }}>
              Use-case walkthroughs: agent patterns, CI, app hosting, computer use.
            </p>
          </a>
        </div>

        <h2>Quickstart</h2>
        <p>
          Create an API key on the <a href="#/keys">Keys page</a>, then pick a client. Full details
          for each are in the <a href="/sdk" target="_blank" rel="noreferrer">SDK docs</a>.
        </p>

        <h3>Python</h3>
        <pre>{`pip install pilldrops

from pilldrops import Sandbox

with Sandbox.create(template="python-3.11") as sb:   # template optional
    r = sb.run("python3 -c 'print(2+2)'")
    print(r.stdout)   # 4`}</pre>

        <h3>JavaScript</h3>
        <pre>{`npm install @pilldrops/sdk

import { Sandbox } from '@pilldrops/sdk'

const sb = await Sandbox.create({ template: 'python-3.11' })
const r = await sb.run("python3 -c 'print(2+2)'")
console.log(r.stdout)   // 4
await sb.kill()`}</pre>

        <h3>CLI</h3>
        <pre>{`npm install -g @pilldrops/cli
pilldrops login --api-url ${serverOrigin()} --key sf_...
pilldrops sandbox create --template python-3.11
pilldrops exec <id> -- python3 -c 'print(2+2)'`}</pre>

        <h3>Raw HTTP</h3>
        <pre>{`curl -X POST ${serverOrigin()}/v1/sandboxes \\
  -H "Authorization: Bearer $KEY" \\
  -d '{"template":"python-3.11","timeout":600}'`}</pre>
        <p>
          Machine-readable spec: <a href="/openapi.json" target="_blank" rel="noreferrer"><code>/openapi.json</code></a>.
          Agent-facing summary: <a href="/llms.txt" target="_blank" rel="noreferrer"><code>/llms.txt</code></a>.
          MCP server for AI agents: <code>{`claude mcp add --transport http pilldrops ${serverOrigin()}/mcp --header "Authorization: Bearer <key>"`}</code>.
        </p>
      </div>
    </div>
  )
}
