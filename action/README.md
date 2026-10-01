# PillDrops GitHub Action

Run tests, builds or a preview server inside an isolated PillDrops microVM — from
CI, in one step. Your workflow gets the exit code; your PR gets a preview URL.

```yaml
- uses: pilldrops/action@v1
  with:
    api-key: ${{ secrets.PILLDROPS_API_KEY }}
    api-url: ${{ vars.PILLDROPS_API_URL }}
    template: playwright
    run: npx playwright test
```

The step fails when the command fails, so this is a normal required check.

## Two modes

**`run:`** — execute a command and wait for it. The command's exit code becomes
the job's status, and the sandbox is destroyed afterwards (even if the command
failed, so a red build never leaks a billed sandbox).

**`serve:`** — start a long-lived server, return as soon as it is up, and leave
the sandbox running so the preview URL stays live. Requires `port`.

```yaml
- uses: pilldrops/action@v1
  id: preview
  with:
    api-key: ${{ secrets.PILLDROPS_API_KEY }}
    api-url: ${{ vars.PILLDROPS_API_URL }}
    template: node-20
    serve: npm start
    port: 3000
    public: true

- run: echo "Preview: ${{ steps.preview.outputs.preview-url }}"
```

## Inputs

| Input | Default | Notes |
|---|---|---|
| `api-key` | — | **Required.** Store it as a repository secret. |
| `template` | `base` | `playwright`, `code-interpreter`, `docker`, `node-20`, … |
| `run` | — | Command to execute. Mutually exclusive with `serve`. |
| `serve` | — | Command that starts a server. Needs `port`. Implies `keep: true`. |
| `path` | `.` | File or directory to upload. `none` skips the upload. |
| `dest` | `workspace` | Where it lands. Directory uploads are relative to the sandbox home. |
| `port` | — | Port to publish. Required with `serve`; with `run` it gives you a URL for reports. |
| `public` | `false` | `true` makes the preview URL tokenless — anyone with the link can open it. |
| `timeout` | `900` | Sandbox lifetime in seconds. |
| `keep` | `false` | Leave the sandbox running (handy for debugging a failure). |
| `api-url` | — | **Required.** Your PillDrops server's URL. |

## Outputs

| Output | Notes |
|---|---|
| `sandbox-id` | The sandbox that was created. |
| `preview-url` | Preview URL, when a port was published. |
| `exit-code` | Exit code of the command that ran. |

## Behaviour worth knowing

- **The command runs inside the upload.** When `path` is a directory, the action
  `cd`s into it first, so `run: pytest` works on a checked-out repo without you
  writing a `cd`.
- **`$PORT` is set for you.** The action exports the published port to the
  command, so an app that honours `$PORT` binds the right one on every version of
  the sandbox agent.
- **Cleanup is unconditional.** The sandbox is destroyed on success, on failure
  and on error — unless you asked to keep it, or used `serve`.
- **Step summary.** Each run writes a pass/fail summary (with the preview link)
  to the workflow summary page.

## Examples

Playwright suite on every PR, with the HTML report served from the sandbox:

```yaml
name: e2e
on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: pilldrops/action@v1
        with:
          api-key: ${{ secrets.PILLDROPS_API_KEY }}
          api-url: ${{ vars.PILLDROPS_API_URL }}
          template: playwright
          run: npx playwright test
          port: 8000        # report + trace + video at the preview URL
```

Smoke-test a staging API and fail the build on a bad response:

```yaml
- uses: pilldrops/action@v1
  with:
    api-key: ${{ secrets.PILLDROPS_API_KEY }}
    api-url: ${{ vars.PILLDROPS_API_URL }}
    template: base
    path: none
    run: curl -sf https://staging.example.com/health
```

Comment the preview URL on the pull request:

```yaml
- uses: pilldrops/action@v1
  id: preview
  with:
    api-key: ${{ secrets.PILLDROPS_API_KEY }}
    api-url: ${{ vars.PILLDROPS_API_URL }}
    template: node-20
    serve: npm start
    port: 3000
    public: true

- run: gh pr comment "$NUMBER" --body "Preview: ${{ steps.preview.outputs.preview-url }}"
  env:
    GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    NUMBER: ${{ github.event.number }}
```

## Publishing this action

GitHub resolves `uses: pilldrops/action@v1` to a repository, so this directory
has to be mirrored to its own repo before that reference works:

1. Create `github.com/pilldrops/action` (public).
2. Copy `action.yml`, `run.sh` and this README to its root.
3. Tag a release and move the floating tag:
   ```sh
   git tag v1.0.0 && git tag -f v1 && git push --tags --force
   ```

Until then, reference it by path from inside this repo: `uses: ./action`.
