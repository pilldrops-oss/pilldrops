# sandboxd sample web app

A tiny stdlib notes API that binds `$PORT` (the sandboxd contract).

## Deploy via the CLI

```bash
export PILLDROPS_API_URL=http://localhost:8080
export PILLDROPS_API_KEY=<your key>

./cli/pilldrops deploy examples/sample-webapp --start "python3 app.py" --port 8000
# → prints an App URL like http://localhost:8080/preview/sb_.../8000/
```

Open the App URL: the notes page renders, and `GET /api/notes` returns JSON.
