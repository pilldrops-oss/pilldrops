// Copy-paste integration snippets per (use case, client). Every snippet must use only the
// verified public surface; a recipe that invents a flag teaches the user the product lies.

import { absolutePreviewUrl, serverOrigin } from '../api'

export const KEY_PLACEHOLDER = '<mint a key above>'

export type Client = 'python' | 'javascript' | 'cli' | 'rest'

export interface IntegrationRecipe {
  id: string
  title: string
  outcome: string
  /** Absent means this client has no honest path yet. */
  snippets: Partial<Record<Client, (key: string) => string>>
  /** What happens when they run it, and the follow-ups. */
  after: string
}

export const CLIENT_LABELS: Record<Client, string> = {
  python: 'Python',
  javascript: 'JavaScript',
  cli: 'CLI',
  rest: 'REST',
}

const env = (key: string) => `# .env\nPILLDROPS_API_URL=${serverOrigin()}\nPILLDROPS_API_KEY=${key}`

export const INTEGRATION_RECIPES: IntegrationRecipe[] = [
  {
    id: 'invoke-from-code',
    title: 'Run code from my app',
    outcome: 'Your program creates a sandbox, runs a command in it, reads the output, cleans up.',
    snippets: {
      python: key => `${env(key)}

# pip install pilldrops
from pilldrops import Sandbox

with Sandbox.create(template="python-3.11") as sb:
    r = sb.run("python3 -c 'print(2+2)'")
    print(r.stdout)   # 4
# the context manager destroys the sandbox on exit`,
      javascript: key => `${env(key)}

// npm install @pilldrops/sdk
import { Sandbox } from '@pilldrops/sdk'

const sb = await Sandbox.create({ template: 'python-3.11' })
const r = await sb.run("python3 -c 'print(2+2)'")
console.log(r.stdout)   // 4
await sb.kill()          // destroy when done — idle sandboxes hold resources`,
      cli: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}
# npm install -g @pilldrops/cli   (once)

pilldrops sandbox create --template python-3.11
pilldrops exec <id> -- python3 -c 'print(2+2)'
pilldrops sandbox destroy <id>`,
      rest: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}

curl -X POST ${serverOrigin()}/v1/sandboxes \\
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \\
  -d '{"template":"python-3.11","timeout":600}'
# → {"id":"sb-…","status":"running",…}

curl -X POST ${serverOrigin()}/v1/sandboxes/<id>/exec \\
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \\
  -d '{"cmd":"python3 -c \\'print(2+2)\\'"}'`,
    },
    after:
      'The exec returns {stdout, stderr, exit_code}. List what’s running with `pilldrops sandbox list`, read logs with `pilldrops sandbox logs <id>`, and destroy what you’re done with — idle sandboxes hold resources.',
  },
  {
    id: 'run-github-repo',
    title: 'Run my GitHub repo in a sandbox',
    outcome: 'A sandbox clones your repo at boot, installs, starts it, and serves it on a public URL.',
    snippets: {
      rest: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}

curl -X POST ${serverOrigin()}/v1/sandboxes \\
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \\
  -d '{
    "template": "node-20",
    "repo": "https://github.com/you/your-app",
    "setup": ["cd pd-repo && npm install"],
    "start": "cd pd-repo && npm start",
    "port": 8000
  }'
# → app_url is your live preview; deploy_log says what each step did`,
      cli: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}

pilldrops sandbox create --template node-20
pilldrops exec <id> -- git clone --depth 1 https://github.com/you/your-app pd-repo
pilldrops exec <id> -- sh -c 'cd pd-repo && npm install && PORT=8000 npm start &'
# preview: ${absolutePreviewUrl('<id>', 8000)}
pilldrops sandbox logs <id>   # if it isn't serving, this says why`,
      python: key => `${env(key)}

# pip install pilldrops
from pilldrops import Sandbox

sb = Sandbox.create(template="node-20")
sb.run("git clone --depth 1 https://github.com/you/your-app pd-repo")
sb.run("cd pd-repo && npm install")
sb.run("cd pd-repo && PORT=8000 nohup npm start &")
# preview: ${absolutePreviewUrl('<sandbox id>', 8000)}`,
    },
    after:
      'Your app must bind 0.0.0.0 and the injected $PORT (default 8000) — hardcoded localhost ports are the #1 cause of a dead preview. A private repo authenticates by passing GIT_TOKEN in env; the token is never interpolated into a command.',
  },
  {
    id: 'agent-from-code',
    title: 'Drive an AI coding agent from my code',
    outcome: 'Your program gives Claude Code a machine and a task; the agent does real work in it.',
    snippets: {
      cli: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}

# One-shot: describe it, get a working URL back
pilldrops ai "a flask todo API with add and list endpoints"

# Or drive the agent explicitly:
pilldrops sandbox create --template claude
pilldrops exec <id> -- claude -p "add tests to src/ and run them"
pilldrops sandbox logs <id>`,
      python: key => `${env(key)}

# pip install pilldrops
from pilldrops import Sandbox

sb = Sandbox.create(template="claude")   # the agent needs your ANTHROPIC_API_KEY
r = sb.run('claude -p "clone github.com/you/your-app and make the tests pass"')
print(r.stdout)`,
      rest: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}

curl -X POST ${serverOrigin()}/v1/sandboxes \\
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \\
  -d '{"template":"claude","args":{"task":"add tests to src/ and run them"}}'`,
    },
    after:
      'The agent runs on YOUR provider key: store ANTHROPIC_API_KEY in the Vault (dashboard → Vault) and it is injected at create via secret_refs — the platform never supplies model keys. Watch progress with `pilldrops sandbox logs <id>`.',
  },
  {
    id: 'browser-tests-ci',
    title: 'Run browser tests from CI',
    outcome: 'Your pipeline gets a throwaway Chromium + Playwright box per run — no grid to maintain.',
    snippets: {
      cli: key => `# in CI: set PILLDROPS_API_KEY=${key} as a secret, and PILLDROPS_API_URL=${serverOrigin()}
npm install -g @pilldrops/cli

pilldrops sandbox create --template playwright
pilldrops exec <id> -- git clone --depth 1 https://github.com/you/your-app pd-repo
pilldrops exec <id> -- sh -c 'cd pd-repo && npm ci && npx playwright test'
pilldrops sandbox destroy <id>   # every run gets a fresh box`,
      javascript: key => `${env(key)}

// npm install @pilldrops/sdk
import { Sandbox } from '@pilldrops/sdk'

const sb = await Sandbox.create({ template: 'playwright' })
await sb.run('git clone --depth 1 https://github.com/you/your-app pd-repo')
const r = await sb.run('cd pd-repo && npm ci && npx playwright test')
console.log(r.stdout)
await sb.kill()
process.exit(r.exit_code)   // the suite’s verdict IS the job’s verdict`,
    },
    after:
      'exit_code carries the suite result, so the CI job fails when the tests do. The exec output is the full reporter output — archive it as the job log.',
  },
  {
    id: 'custom-tool',
    title: 'Run a tool we don’t stock',
    outcome: 'Locust, your profiler, anything — installed at create, or brought as your own image.',
    snippets: {
      cli: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}

# Simplest: a base sandbox + install your tool at create
pilldrops sandbox create --template python-3.11
pilldrops exec <id> -- pip install locust
pilldrops exec <id> -- git clone --depth 1 https://github.com/you/your-app pd-repo
pilldrops exec <id> -- sh -c 'cd pd-repo && locust -f locustfile.py --headless -u 50 -r 10 --run-time 2m --host https://your-target'
pilldrops sandbox logs <id>`,
      rest: key => `export PILLDROPS_API_URL=${serverOrigin()} PILLDROPS_API_KEY=${key}

# Option A — install at create: setup runs before start
curl -X POST ${serverOrigin()}/v1/sandboxes \
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \
  -d '{
    "template": "python-3.11",
    "repo": "https://github.com/you/your-app",
    "setup": ["pip install locust"],
    "start": "cd pd-repo && locust -f locustfile.py --web-port $PORT",
    "port": 8089
  }'

# Option B — bring your own Docker image, no template needed
curl -X POST ${serverOrigin()}/v1/sandboxes \
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \
  -d '{"image": "locustio/locust", "port": 8089}'

# Option C — build and publish an image so it's one click for the team
# It builds on ONE node and publishes to the fleet; check .ok and .publish.ok
# (body is a tar build context containing your Dockerfile)
tar czf - . | curl -X POST "${serverOrigin()}/v1/images/build?name=locust" \
  -H "Authorization: Bearer $PILLDROPS_API_KEY" \
  --data-binary @-`,
      python: key => `${env(key)}

# pip install pilldrops
from pilldrops import Sandbox

sb = Sandbox.create(template="python-3.11")
sb.run("pip install locust")
sb.run("git clone --depth 1 https://github.com/you/your-app pd-repo")
r = sb.run("cd pd-repo && locust -f locustfile.py --headless -u 50 -r 10 --run-time 2m --host https://your-target")
print(r.stdout)`,
    },
    after:
      'Install-at-create keeps you on a stock base (fastest to iterate); your own image gives you the exact toolchain; building one (POST /v1/images/build) makes it one-click for the whole team afterwards — it builds on one node and publishes to the fleet, so read publish.ok, not just ok.',
  },
  {
    id: 'ai-assistant-operator',
    title: 'Let my AI assistant operate sandboxes',
    outcome: 'Claude/Cursor/ChatGPT drives PillDrops end-to-end — you talk, it provisions.',
    snippets: {
      cli: key => `# Give your assistant the operator prompt (dashboard → Catalog → “Copy prompt”),
# or connect the MCP server directly:
claude mcp add --transport http pilldrops ${serverOrigin()}/mcp --header "Authorization: Bearer ${key}"

# It will sign in with your key:
pilldrops login --api-url ${serverOrigin()} --key ${key}
pilldrops doctor        # verifies auth + connectivity
pilldrops ai --json     # the full machine-readable command catalog`,
    },
    after:
      'From there you describe what you want in your own assistant; it creates, deploys, checks the preview and hands you a URL. `/llms.txt` is the agent-facing summary of the whole API.',
  },
]

/** integrationGrounding lists recipes with one canonical snippet each for the BYOK advisor; never contains a credential. */
export function integrationGrounding(): string[] {
  const canonical = INTEGRATION_RECIPES.map(r => {
    const pick = r.snippets.python ?? r.snippets.cli ?? r.snippets.rest ?? r.snippets.javascript
    return `${r.title} — ${r.outcome}\n${pick ? pick('$PILLDROPS_API_KEY').slice(0, 700) : ''}`
  })
  canonical.push(
    `REST surface (any language): base ${serverOrigin()}/v1 · POST /sandboxes {"template","repo","setup","start","port"} · POST /sandboxes/{id}/exec {"cmd"} · GET /sandboxes/{id}/logs · DELETE /sandboxes/{id} · Authorization: Bearer $PILLDROPS_API_KEY`,
  )
  return canonical
}
