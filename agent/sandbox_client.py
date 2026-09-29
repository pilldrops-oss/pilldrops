"""Thin client for the sandbox platform REST API; depends only on requests."""
import requests
from dataclasses import dataclass
from typing import Optional


@dataclass
class ExecResult:
    stdout: str
    stderr: str
    exit_code: int
    truncated: bool = False

    @property
    def ok(self) -> bool:
        return self.exit_code == 0

    def raise_on_error(self):
        if not self.ok:
            raise RuntimeError(
                f"Command failed (exit {self.exit_code}):\n{self.stderr or self.stdout}"
            )
        return self


@dataclass
class SandboxInfo:
    id: str
    template: str
    boot_ms: int
    boot_type: str
    workspace: str
    url: str
    api_exec: str
    api_files: str


class SandboxClient:
    def __init__(self, base_url: str = "http://localhost:8080"):
        self.base_url = base_url.rstrip("/")
        self.session = requests.Session()
        self.session.headers["Content-Type"] = "application/json"

    def health(self) -> bool:
        r = self.session.get(f"{self.base_url}/health", timeout=5)
        return r.ok

    def create(
        self, template: str = "python-3.11", timeout: int = 600, public: bool = False
    ) -> SandboxInfo:
        """Create a sandbox. public=True makes the preview URL reachable without a token."""
        body = {"template": template, "timeout": timeout}
        if public:
            body["public"] = True
        r = self.session.post(
            f"{self.base_url}/v1/sandboxes",
            json=body,
            timeout=30,
        )
        r.raise_for_status()
        d = r.json()
        # The create response carries no URLs; fetch the full record.
        info = self.info(d["id"])
        return info

    def info(self, sandbox_id: str) -> SandboxInfo:
        r = self.session.get(
            f"{self.base_url}/v1/sandboxes/{sandbox_id}", timeout=10
        )
        r.raise_for_status()
        d = r.json()
        return SandboxInfo(
            id=d["id"],
            template=d.get("template", ""),
            boot_ms=d.get("boot_ms", 0),
            boot_type=d.get("boot_type", ""),
            workspace=d.get("workspace", ""),
            url=d.get("url", ""),
            api_exec=d.get("api_exec", ""),
            api_files=d.get("api_files", ""),
        )

    def exec(
        self, sandbox_id: str, cmd: str, cwd: str = "", timeout: int = 30
    ) -> ExecResult:
        r = self.session.post(
            f"{self.base_url}/v1/sandboxes/{sandbox_id}/exec",
            json={"cmd": cmd, "cwd": cwd, "timeout": timeout},
            timeout=timeout + 15,
        )
        r.raise_for_status()
        d = r.json()
        return ExecResult(
            stdout=d.get("stdout", ""),
            stderr=d.get("stderr", ""),
            exit_code=d.get("exit_code", -1),
            truncated=d.get("truncated", False),
        )

    def write_file(self, sandbox_id: str, path: str, content: str) -> None:
        if isinstance(content, str):
            content = content.encode()
        r = self.session.post(
            f"{self.base_url}/v1/sandboxes/{sandbox_id}/files",
            params={"path": path},
            data=content,
            headers={"Content-Type": "application/octet-stream"},
            timeout=30,
        )
        r.raise_for_status()

    def read_file(self, sandbox_id: str, path: str) -> str:
        r = self.session.get(
            f"{self.base_url}/v1/sandboxes/{sandbox_id}/files",
            params={"path": path},
            timeout=30,
        )
        r.raise_for_status()
        return r.text

    def destroy(self, sandbox_id: str) -> None:
        self.session.delete(
            f"{self.base_url}/v1/sandboxes/{sandbox_id}", timeout=10
        )

    def list(self) -> list:
        r = self.session.get(f"{self.base_url}/v1/sandboxes", timeout=10)
        r.raise_for_status()
        return r.json()
