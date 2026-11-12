"""PillDrops driver for the contract tests; drives the live fleet through the released pilldrops CLI.
Auth: PILLDROPS_API_KEY / PILLDROPS_API_URL, falling back to the CLI's stored login (~/.pilldrops/config.json).
"""

import json
import os
import subprocess
import tempfile
import time
from urllib.parse import urlparse


class ExecResult:
    def __init__(self, exit_code, stdout, stderr, dur):
        self.exit_code, self.stdout, self.stderr, self.dur = exit_code, stdout, stderr, dur

    def __repr__(self):
        return f"ExecResult(exit={self.exit_code}, dur={self.dur:.2f}s)"


# The sandbox domain a create in this run returned a host-form preview URL on.
_learned_domain = None


def _learn_domain(meta):
    """Remember the domain of the first <port>-<id>.<domain> URL in a create response."""
    global _learned_domain
    for key in ("app_url", "url", "viewer_url"):
        host = urlparse(meta.get(key) or "").hostname or ""
        label, _, rest = host.partition(".")
        port, dash, _ = label.partition("-")
        if rest and dash and port.isdigit():
            _learned_domain = rest
            return


def sandbox_domain():
    """The deployment's sandbox domain, where previews live as <port>-<id>.<domain>.

    PILLDROPS_SANDBOX_DOMAIN, else the domain of a preview URL a create in this
    run returned, else the host of PILLDROPS_API_URL, else the host of the CLI's
    stored login: the suite targets whatever deployment it is pointed at, never
    a built-in one, and previews may live on a different domain from the API.
    """
    if os.environ.get("PILLDROPS_SANDBOX_DOMAIN"):
        return os.environ["PILLDROPS_SANDBOX_DOMAIN"]
    if _learned_domain:
        return _learned_domain
    url = os.environ.get("PILLDROPS_API_URL")
    if not url:
        try:
            with open(os.path.expanduser("~/.pilldrops/config.json")) as f:
                url = json.load(f).get("api_url")
        except (OSError, ValueError):
            url = None
    if not url:
        raise RuntimeError("set PILLDROPS_API_URL (or PILLDROPS_SANDBOX_DOMAIN) to the deployment under test")
    return urlparse(url).hostname


def _run(cmd, timeout=300, input_=None):
    return subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, input=input_)


class PDDriver:
    name = "pilldrops"

    def __init__(self, template="python-3.11", timeout=600, envs=None, ports=None,
                 start=None, public=False):
        cmd = ["pilldrops", "--json", "sandbox", "create", "--template", template,
               "--timeout", str(timeout)]
        for k, v in (envs or {}).items():
            cmd += ["--env", f"{k}={v}"]
        for p in (ports or []):
            cmd += ["--port", str(p)]
        if start:
            cmd += ["--start", start]
        if public:
            # Previews are token-gated by default; -public needs CLI >= v1.7.0.
            cmd += ["-public"]
        t0 = time.time()
        r = _run(cmd, timeout=180)
        self.create_dur = time.time() - t0
        if r.returncode != 0:
            raise RuntimeError(f"pilldrops create failed: {r.stdout} {r.stderr}")
        self.meta = json.loads(r.stdout)
        self.sandbox_id = self.meta["id"]
        _learn_domain(self.meta)

    def exec(self, cmd, timeout=300, envs=None):
        if envs:
            prefix = " ".join(f"{k}='{v}'" for k, v in envs.items())
            cmd = f"export {prefix}; {cmd}"
        t0 = time.time()
        r = _run(["pilldrops", "exec", self.sandbox_id, "--", "bash", "-lc", cmd],
                 timeout=timeout + 30)
        return ExecResult(r.returncode, r.stdout, r.stderr, time.time() - t0)

    def start(self, cmd, port=None):
        args = ["pilldrops", "start", self.sandbox_id]
        if port:
            args += ["--port", str(port)]
        args += ["--", "bash", "-lc", cmd]
        r = _run(args, timeout=120)
        if r.returncode != 0:
            raise RuntimeError(f"start failed: {r.stdout} {r.stderr}")
        return r.stdout

    def write_file(self, path, content):
        t0 = time.time()
        with tempfile.NamedTemporaryFile("w", suffix=os.path.basename(path),
                                         delete=False) as f:
            f.write(content)
            local = f.name
        r = _run(["pilldrops", "cp", local, f"{self.sandbox_id}:{path}"], timeout=120)
        os.unlink(local)
        if r.returncode != 0:
            raise RuntimeError(f"cp failed: {r.stderr}")
        return time.time() - t0

    def read_file(self, path):
        local = tempfile.mktemp()
        r = _run(["pilldrops", "cp", f"{self.sandbox_id}:{path}", local], timeout=120)
        if r.returncode != 0:
            raise RuntimeError(f"cp back failed: {r.stderr}")
        with open(local) as f:
            data = f.read()
        os.unlink(local)
        return data

    def preview_url(self, port):
        return f"https://{port}-{self.sandbox_id}.{sandbox_domain()}"

    def pause(self):
        r = _run(["pilldrops", "sandbox", "pause", self.sandbox_id], timeout=120)
        if r.returncode != 0:
            raise RuntimeError(r.stderr or r.stdout)

    def resume(self):
        r = _run(["pilldrops", "sandbox", "resume", self.sandbox_id], timeout=120)
        if r.returncode != 0:
            raise RuntimeError(r.stderr or r.stdout)

    def destroy(self):
        _run(["pilldrops", "sandbox", "rm", self.sandbox_id], timeout=60)


def cli_authed():
    try:
        r = _run(["pilldrops", "--json", "whoami"], timeout=30)
        return r.returncode == 0
    except (FileNotFoundError, subprocess.TimeoutExpired):
        return False
