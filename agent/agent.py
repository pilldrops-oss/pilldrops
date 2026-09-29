#!/usr/bin/env python3
"""AI code agent: creates a sandbox, writes code, runs it, and iterates until it works.

Usage: python agent.py "build a todo API with Flask" (needs ANTHROPIC_API_KEY), or python agent.py --plan plan.json
"""
import argparse
import json
import os
import sys
import time
from sandbox_client import SandboxClient, ExecResult

G = "\033[32m"  # green
R = "\033[31m"  # red
O = "\033[33m"  # orange
C = "\033[36m"  # cyan
B = "\033[1m"   # bold
N = "\033[0m"   # reset


def log(icon: str, msg: str):
    print(f"  {icon}  {msg}")


def run_plan(client: SandboxClient, plan: dict, sandbox_id: str) -> dict:
    results = {"files_written": [], "commands": [], "output": "", "error": None}

    for f in plan.get("files", []):
        path, content = f["path"], f["content"]
        client.write_file(sandbox_id, path, content)
        results["files_written"].append(path)
        log("📄", f"Wrote {C}{path}{N}")

    for cmd in plan.get("setup_commands", []):
        log("⚙️ ", f"Running: {C}{cmd}{N}")
        r = client.exec(sandbox_id, cmd, timeout=120)
        results["commands"].append({"cmd": cmd, "exit_code": r.exit_code})
        if not r.ok:
            log("❌", f"{R}Setup failed (exit {r.exit_code}): {r.stderr[:200]}{N}")
            results["error"] = {"cmd": cmd, "exit_code": r.exit_code, "stderr": r.stderr, "stdout": r.stdout}
            return results
        if r.stdout.strip():
            for line in r.stdout.strip().split("\n")[-3:]:
                log("  ", f"  {line}")

    run_cmd = plan.get("run_command", "")
    if run_cmd:
        log("🚀", f"Running: {O}{run_cmd}{N}")
        r = client.exec(sandbox_id, run_cmd, timeout=60)
        results["commands"].append({"cmd": run_cmd, "exit_code": r.exit_code})
        results["output"] = r.stdout
        if not r.ok:
            log("❌", f"{R}Failed (exit {r.exit_code}){N}")
            if r.stderr:
                for line in r.stderr.strip().split("\n")[-5:]:
                    log("  ", f"  {R}{line}{N}")
            results["error"] = {"cmd": run_cmd, "exit_code": r.exit_code, "stderr": r.stderr, "stdout": r.stdout}
        else:
            if r.stdout.strip():
                log("✅", f"{G}Output:{N}")
                for line in r.stdout.strip().split("\n"):
                    log("  ", f"  {G}{line}{N}")

    return results


def call_claude(prompt: str, system: str) -> dict:
    """Call Claude API and parse the JSON plan from the response."""
    try:
        import anthropic
    except ImportError:
        print(f"\n{R}Error: anthropic package not installed.{N}")
        print(f"Run: pip install anthropic")
        print(f"Or use --plan flag with a JSON plan file instead.")
        sys.exit(1)

    api_key = os.environ.get("ANTHROPIC_API_KEY")
    if not api_key:
        print(f"\n{R}Error: ANTHROPIC_API_KEY not set.{N}")
        print(f"Export your API key: export ANTHROPIC_API_KEY=sk-ant-...")
        print(f"Or use --plan flag with a JSON plan file instead.")
        sys.exit(1)

    client = anthropic.Anthropic(api_key=api_key)
    response = client.messages.create(
        model="claude-sonnet-4-20250514",
        max_tokens=4096,
        system=system,
        messages=[{"role": "user", "content": prompt}],
    )

    text = response.content[0].text
    if "```json" in text:
        text = text.split("```json")[1].split("```")[0]
    elif "```" in text:
        text = text.split("```")[1].split("```")[0]

    return json.loads(text.strip())


def main():
    parser = argparse.ArgumentParser(description="AI Code Agent — Run code in sandboxes")
    parser.add_argument("prompt", nargs="?", help="Natural language task description")
    parser.add_argument("--plan", help="Path to a JSON plan file (no AI needed)")
    parser.add_argument("--url", default="http://localhost:8080", help="Sandbox API URL")
    parser.add_argument("--template", default="python-3.11", help="Sandbox template")
    parser.add_argument("--max-retries", type=int, default=3, help="Max fix iterations")
    parser.add_argument("--keep", action="store_true", help="Keep sandbox after completion")
    args = parser.parse_args()

    if not args.prompt and not args.plan:
        parser.print_help()
        print(f"\n{O}Examples:{N}")
        print(f'  python agent.py "build a todo API with Flask"')
        print(f'  python agent.py --plan examples/plans/todo_api.json')
        sys.exit(1)

    client = SandboxClient(args.url)

    try:
        if not client.health():
            raise Exception("unhealthy")
    except Exception:
        print(f"\n{R}Error: Cannot connect to sandbox platform at {args.url}{N}")
        print(f"Start it with: make dev-full")
        sys.exit(1)

    print(f"\n{B}{O}╔══════════════════════════════════════════╗{N}")
    print(f"{B}{O}║  AI Code Agent                           ║{N}")
    print(f"{B}{O}╚══════════════════════════════════════════╝{N}\n")

    if args.plan:
        log("📋", f"Loading plan from {C}{args.plan}{N}")
        with open(args.plan) as f:
            plan = json.load(f)
    else:
        log("🤖", f"Asking Claude to plan: {C}{args.prompt}{N}")
        from prompts import SYSTEM_PROMPT, FIX_PROMPT
        plan = call_claude(args.prompt, SYSTEM_PROMPT)

    if plan.get("description"):
        log("📝", f"Plan: {plan['description']}")

    log("📦", f"Files: {', '.join(f['path'] for f in plan.get('files', []))}")
    log("⚙️ ", f"Setup: {len(plan.get('setup_commands', []))} commands")
    log("🚀", f"Run: {plan.get('run_command', '(none)')}")
    print()

    log("🔧", f"Creating sandbox ({C}{args.template}{N})...")
    sandbox = client.create(template=args.template)
    log("✅", f"{G}Sandbox ready: {sandbox.id} ({sandbox.boot_ms}ms){N}")
    log("🔗", f"URL: {C}{sandbox.url}{N}")
    print()

    attempt = 0
    while attempt <= args.max_retries:
        if attempt > 0:
            print(f"\n{O}── Retry {attempt}/{args.max_retries} ──{N}\n")

        results = run_plan(client, plan, sandbox.id)

        if results["error"] is None:
            break

        if attempt >= args.max_retries:
            log("💥", f"{R}Max retries reached. Last error:{N}")
            log("  ", f"  {R}{results['error']['stderr'][:300]}{N}")
            break

        if args.prompt and not args.plan:
            log("🔄", f"Asking Claude to fix the error...")
            from prompts import FIX_PROMPT
            err = results["error"]
            file_list_result = client.exec(sandbox.id, "find . -type f -name '*.py' -o -name '*.txt' -o -name '*.json' | head -20")
            fix_prompt = FIX_PROMPT.format(
                command=err["cmd"],
                exit_code=err["exit_code"],
                stdout=err["stdout"][:500],
                stderr=err["stderr"][:500],
                file_list=file_list_result.stdout,
            )
            plan = call_claude(fix_prompt, "You are a code debugging agent. Fix the error.")
            log("📋", f"Got fix plan with {len(plan.get('files', []))} file changes")
        else:
            log("⚠️ ", f"{O}Cannot auto-fix in --plan mode. Stopping.{N}")
            break

        attempt += 1

    print(f"\n{'═' * 50}")
    if results["error"] is None:
        print(f"{G}{B}  ✅ Success!{N}")
    else:
        print(f"{R}{B}  ❌ Failed after {attempt} attempts{N}")

    print(f"\n  Sandbox:   {C}{sandbox.id}{N}")
    print(f"  URL:       {C}{sandbox.url}{N}")
    print(f"  API Exec:  {C}{sandbox.api_exec}{N}")
    print(f"  API Files: {C}{sandbox.api_files}{N}")
    print(f"  Dashboard: {C}{args.url}/dashboard{N}")

    if not args.keep:
        print(f"\n  {O}Sandbox will auto-destroy on timeout.{N}")
        print(f"  Use --keep to prevent cleanup, or destroy manually:")
        print(f"  curl -X DELETE {args.url}/v1/sandboxes/{sandbox.id}")
    else:
        print(f"\n  {G}Sandbox kept alive. Destroy when done:{N}")
        print(f"  curl -X DELETE {args.url}/v1/sandboxes/{sandbox.id}")

    print()


if __name__ == "__main__":
    main()
