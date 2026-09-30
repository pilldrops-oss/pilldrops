import{j as e}from"./index-B1wSnZSK.js";import{Y as r}from"./api-DU4EsTSJ.js";const s={display:"block",padding:"18px 20px",border:"1px solid var(--hair, rgba(128,128,128,.25))",borderRadius:10,textDecoration:"none",color:"inherit"};function a(){return e.jsxs("div",{className:"wrap",children:[e.jsx("div",{className:"h1row",children:e.jsxs("div",{children:[e.jsx("h1",{className:"h1",children:"Docs"}),e.jsx("p",{children:"SDK guides, the runnable API reference, and use-case recipes"})]})}),e.jsxs("div",{className:"docs-content",children:[e.jsxs("div",{style:{display:"grid",gridTemplateColumns:"repeat(auto-fit, minmax(220px, 1fr))",gap:14,margin:"18px 0 8px"},children:[e.jsxs("a",{href:"/sdk",target:"_blank",rel:"noreferrer",style:s,children:[e.jsx("h3",{style:{margin:"0 0 6px"},children:"SDK documentation →"}),e.jsxs("p",{style:{margin:0},children:["Python & JavaScript SDKs + CLI: install, auth, every ",e.jsx("code",{children:"Sandbox"})," method, preview URLs, custom images, common workflows."]})]}),e.jsxs("a",{href:"/docs",target:"_blank",rel:"noreferrer",style:s,children:[e.jsx("h3",{style:{margin:"0 0 6px"},children:"API reference →"}),e.jsx("p",{style:{margin:0},children:"Every endpoint, runnable in the browser — paste a key and hit the live API from the page."})]}),e.jsxs("a",{href:"/guides",target:"_blank",rel:"noreferrer",style:s,children:[e.jsx("h3",{style:{margin:"0 0 6px"},children:"Guides →"}),e.jsx("p",{style:{margin:0},children:"Use-case walkthroughs: agent patterns, CI, app hosting, computer use."})]})]}),e.jsx("h2",{children:"Quickstart"}),e.jsxs("p",{children:["Create an API key on the ",e.jsx("a",{href:"#/keys",children:"Keys page"}),", then pick a client. Full details for each are in the ",e.jsx("a",{href:"/sdk",target:"_blank",rel:"noreferrer",children:"SDK docs"}),"."]}),e.jsx("h3",{children:"Python"}),e.jsx("pre",{children:`pip install pilldrops

from pilldrops import Sandbox

with Sandbox.create(template="python-3.11") as sb:   # template optional
    r = sb.run("python3 -c 'print(2+2)'")
    print(r.stdout)   # 4`}),e.jsx("h3",{children:"JavaScript"}),e.jsx("pre",{children:`npm install @pilldrops/sdk

import { Sandbox } from '@pilldrops/sdk'

const sb = await Sandbox.create({ template: 'python-3.11' })
const r = await sb.run("python3 -c 'print(2+2)'")
console.log(r.stdout)   // 4
await sb.kill()`}),e.jsx("h3",{children:"CLI"}),e.jsx("pre",{children:`npm install -g @pilldrops/cli
pilldrops login --api-url ${r()} --key sf_...
pilldrops sandbox create --template python-3.11
pilldrops exec <id> -- python3 -c 'print(2+2)'`}),e.jsx("h3",{children:"Raw HTTP"}),e.jsx("pre",{children:`curl -X POST ${r()}/v1/sandboxes \\
  -H "Authorization: Bearer $KEY" \\
  -d '{"template":"python-3.11","timeout":600}'`}),e.jsxs("p",{children:["Machine-readable spec: ",e.jsx("a",{href:"/openapi.json",target:"_blank",rel:"noreferrer",children:e.jsx("code",{children:"/openapi.json"})}),". Agent-facing summary: ",e.jsx("a",{href:"/llms.txt",target:"_blank",rel:"noreferrer",children:e.jsx("code",{children:"/llms.txt"})}),". MCP server for AI agents: ",e.jsx("code",{children:`claude mcp add --transport http pilldrops ${r()}/mcp --header "Authorization: Bearer <key>"`}),"."]})]})]})}export{a as default};
