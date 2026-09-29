# Template registry — publish images, no code changes

sandboxd discovers templates from disk at runtime. Publishing a new image is
enough to make it selectable everywhere (API validation, `/v1/templates`, the
dashboard picker, `pdctl templates`):

```
/var/lib/sandbox/templates/<id>/rootfs.squashfs   # the built image (required)
/var/lib/sandbox/templates/<id>/template.json     # optional manifest
```

No Go catalog edit, no binary rebuild, no daemon restart — the registry
rescans the directory (5s TTL) on every catalog read.

## Publishing a template

1. Author it under `templates/<id>/` in this repo:
   - `Dockerfile` — the toolchain. Everything must be baked in; users never
     run install steps.
   - `start.sh` (optional) — a persistent app started at boot. It must bind
     `$PORT` (8000 — the only app port the preview proxy routes to).
   - `template.json` (optional) — display metadata + resources + app port.
2. Build it on the host: `sudo bash scripts/build-template.sh <id>`.
   The script bakes the Docker image, packs the squashfs, and publishes the
   manifest next to it.
3. Done. It appears in the dashboard picker and `pdctl templates` within 5s.

Compiled-in catalog entries (`internal/templates/catalog.go`) still exist for
the core templates; a published `template.json` with the same id overrides the
compiled-in entry field-by-field. To pool-warm a template (pre-booted VM,
instant create), add it to the daemon's `TEMPLATE` env — everything else works
without it (cold boot, ~1–2s).

## Manifest schema (template.json)

Same JSON shape as a catalog entry; all fields optional, sane floors applied:

```json
{
  "name": "Claude Terminal",
  "description": "Claude Code CLI in a browser terminal",
  "language": "shell",
  "icon": "ct",
  "tags": ["agent", "terminal"],
  "cpu": 2,
  "memory_mb": 2048,
  "storage_mb": 4096,
  "port": 8080,
  "visibility": "public"
}
```

`port` is the contract that makes "select template → click link" work: it is
published as the sandbox's app port at create time, so preview links route to
the template's built-in app.

## Gallery (built or in-tree)

| template | what you get at the URL |
|---|---|
| `desktop` | Chromium in fluxbox over noVNC (browser-in-a-URL); see §Desktop |
| *any template* | add `PD_DESKTOP=1` at create for a desktop on port 6080 |
| `playwright` | headless Playwright + recordings server (`pwtest`, `pwrecord`) |
| `playwright-headed` | watch tests run live over noVNC; artifacts at `/recordings/` |
| `claude-terminal` | browser terminal (ttyd) with Claude Code CLI preinstalled |
| `code-interpreter` | Python + Jupyter, numpy/pandas/sklearn |
| `claude`, `codex`, `grok`, `opencode`, `amp` | coding-agent CLIs (use via `exec` or pair with ttyd) |
| `docker` | Docker Engine + Compose + Buildx, daemon already running: `docker run -p 8000:80 …` is live on the preview URL. Firecracker only — a Docker-runtime node refuses it at create, see [runtimes.md](runtimes.md) |
| `base`, `node`, `go` | plain toolchains |

## Desktop

A desktop is a capability of every platform image, not a property of four
templates. Any sandbox created with `PD_DESKTOP=1` in its environment gets a
virtual display, a window manager and a viewer:

```
pilldrops sandbox create -t python-3.11 --env PD_DESKTOP=1
pilldrops sandbox open <id>          # prints the viewer URL and opens it
```

- **`desktop_url`** on every create and get response is the link, beside
  `terminal_url` and for the same reason: 6080 is a platform port, so it is
  routable on any sandbox and answers the proxy's "not responding" until
  something binds it.
- **Port 6080**, a platform port like the web terminal's 7681. It is exempt
  from the one-app-port rule, so a sandbox can serve its own app on 8000 *and*
  a desktop at the same time. On a Docker node it is published only when the
  create asked for it.
- **Nothing runs without the flag.** No `Xvfb`, no memory, no published port.
  `pgrep Xvfb` is empty in a sandbox created without it.
- **`has_desktop`** on every create, get and list response says whether this
  sandbox *could* serve one: the node's runtime can host an X server **and**
  this sandbox's image holds the software. A caller-supplied image and an
  image-backed service template read `false`; every platform template reads
  `true`. Clients should enable or grey the Desktop action on this rather
  than opening a preview that shows nothing.
- **The stack**: `Xvfb :0` with the RANDR extension, `fluxbox`, `autocutsel`,
  `x11vnc -shared -xrandr`, and `websockify` serving noVNC. A missing binary
  logs one line and the sandbox still boots without a screen.
- **`DISPLAY=:0`** enters exec and terminal sessions once the display exists,
  so `exec 'xterm &'` appears on the screen someone is watching.
- **Resize**: the display follows the viewer's browser window (`resize=remote`)
  rather than scaling a fixed framebuffer.
- **Clipboard**: `autocutsel` keeps PRIMARY and CLIPBOARD in sync, so noVNC's
  clipboard panel round-trips with `xterm` and Chromium.
- **Shared control, no takeover.** `x11vnc -shared`: every connected viewer
  has input, and a new viewer does not evict an existing one. This is the
  semantics, not a gap to work around; exclusive input would need a websockify
  wrapper that closes the previous client.
- **The launcher** is the fluxbox root menu (right-click the desktop),
  generated at desktop start from the image's own
  `/usr/share/applications/*.desktop` entries plus a terminal.
- **An open viewer keeps the sandbox awake**: the idle timer is refreshed
  while the viewer's WebSocket is held, so a sandbox someone is looking at is
  not paused under them.

The `desktop` template bakes `PD_DESKTOP=1` and adds Chromium plus an
autostart that fills the display; its preview link points at 6080 rather than
at an app port nothing binds.

### Uploading files onto a sandbox

The dashboard's Files tab is the drop target: drop files or a folder onto the
tree, or use its upload buttons. Files are posted one at a time to
`POST /v1/sandboxes/{id}/files`, which streams the body into the sandbox under
its own `PD_FILES_MAX_BYTES` (default 1 GiB) rather than the node's general
`MAX_REQUEST_BYTES`, so a large file is not refused by a cap meant for JSON.
At most 500 files per drop; the upload stops at the first failure and reports
the server's message. Dropping onto the noVNC canvas itself is not supported —
the canvas is the remote screen, not a file target.

## Use-case roadmap

| use case | approach | feasibility |
|---|---|---|
| Claude Code in a terminal in the browser | `claude-terminal` (ttyd) | ✅ shipped |
| Playwright headless + recording + serving | `playwright` | ✅ shipped |
| Playwright headed — watch live + recording | `playwright-headed` (Xvfb + noVNC) | ✅ shipped |
| Any GUI Linux app in a URL | desktop pattern: launch the app instead of Chromium on Xvfb | ✅ pattern proven |
| Claude Desktop (GUI app) in a sandbox | Linux Electron build on the desktop pattern; official app is mac/win only, community Linux builds exist. Practical near-term: claude.ai in the `desktop` template works today | ⚠️ doable, unofficial build |
| VS Code in the browser | `code-server` on the preview port | ✅ easy next template |
| Jupyter as the preview app | `code-interpreter` + start.sh running `jupyter lab --port $PORT` | ✅ easy next template |
| Phone simulator (Android) | Android Emulator needs KVM; Firecracker guests have **no nested virt**, so it would fall back to unusably slow software emulation. Waydroid needs binder/ashmem kernel modules the current guest kernel (5.10.223, measured — `zcat /proc/config.gz` in a booted guest) lacks. Realistic paths: custom guest kernel with binder (waydroid), or run emulators on the host and proxy adb/scrcpy into sandboxes | ❌ blocked today; kernel work required |
| iOS simulator | macOS-only (Xcode); cannot run in Linux microVMs | ❌ not possible |

## Constraints for template authors

- Docker injects `/etc/hosts`/`resolv.conf` at *container* runtime — the boot
  init regenerates them in the VM, don't rely on them at build time.
- Everything runs as root inside the VM; Chromium-family apps need
  `--no-sandbox`.
- The guest has DNS + HTTP(S) egress only (host firewall allowlists 53/80/443).
- One app port per sandbox (the published `port`); put multiple surfaces
  behind one server (see playwright-headed: noVNC + `/recordings/` on 8000).
- The app port is **8000** on every template. It is what a start command binds
  and what the preview proxy routes to by default (`fcAppPort`), so a template
  publishing another *app* port is unreachable; `TestOneAppPort` fails a
  catalogue row that tries. The platform ports are the exception — 7681
  (terminal), 6080 (desktop), 6002 (agent) are reachable on every sandbox
  regardless, and `desktop` declares 6080 as its preview port because its
  surface is the platform viewer rather than an app of its own.
- The desktop stack is added to every template image by
  `scripts/build-template.sh` after the template's own build, so a template
  Dockerfile should not install `xvfb`/`x11vnc`/`novnc`/`fluxbox` itself.
  `PD_NO_DESKTOP=1` skips it for an image `apt` cannot extend — do that only
  alongside removing the template from the catalogue, or `has_desktop` lies.
- The guest root is an **overlayfs** over the read-only squashfs. It does not
  stack, so anything that wants its own overlay mounts (a container daemon's
  storage driver, most obviously) must use `/mnt/disk`, which `overlay-init`
  binds to the raw ext4 of the same writable disk. See `templates/docker`.
