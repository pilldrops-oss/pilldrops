# sandboxd sample web app

A tiny stdlib notes API that binds `$PORT` (the sandboxd contract).

## Deploy via the CLI

```bash
npm install -g @pilldrops/cli
export PILLDROPS_API_URL=https://<your-server>
export PILLDROPS_API_KEY=<your key>

SB=$(pilldrops --json sandbox create --template python-3.11 | jq -r .id)
pilldrops cp -r examples/sample-webapp $SB:app
pilldrops start $SB --port 8000 -- python3 app/sample-webapp/app.py
pilldrops sandbox get $SB    # its url is the App URL
```

Open the App URL: the notes page renders, and `GET /api/notes` returns JSON.
