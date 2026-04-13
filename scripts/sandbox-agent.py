#!/usr/bin/env python3
"""CogOS Sandbox Agent — tool-use loop against local Ollama models.

A lightweight agent that:
  1. Sends prompts to a local model (via Ollama or CogOS kernel)
  2. Parses tool calls from the response
  3. Executes approved tools in a sandbox
  4. Loops until the model calls 'done' or max turns reached

Usage:
    python3 scripts/sandbox-agent.py "Review the project structure"
    python3 scripts/sandbox-agent.py --model gemma4:e4b "Quick check"
    python3 scripts/sandbox-agent.py --sandbox workspace "Fix the typo in README.md"
"""

import argparse
import json
import os
import subprocess
import sys
import time
from pathlib import Path
from urllib.request import Request, urlopen
from urllib.error import URLError

# ── Config ───────────────────────────────────────────────────────────────────

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Read a file's contents (first 200 lines)",
            "parameters": {
                "type": "object",
                "properties": {"path": {"type": "string"}},
                "required": ["path"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "run_command",
            "description": "Run a shell command. In read-only mode: only ls, find, grep, cat, wc, git log/status/diff, go vet/build.",
            "parameters": {
                "type": "object",
                "properties": {"command": {"type": "string"}},
                "required": ["command"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "write_file",
            "description": "Write content to a file (workspace sandbox only)",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string"},
                    "content": {"type": "string"},
                },
                "required": ["path", "content"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "done",
            "description": "Signal task completion with a summary",
            "parameters": {
                "type": "object",
                "properties": {"summary": {"type": "string"}},
                "required": ["summary"],
            },
        },
    },
]

ALLOWED_RO_PREFIXES = [
    "cat ", "head ", "tail ", "less ", "wc ", "ls ", "find ", "grep ", "rg ",
    "git log", "git status", "git diff", "git show", "git blame",
    "go vet", "go build", "python3 -m py_compile", "python3 -c",
    "curl -sf", "ollama list",
]

BLOCKED_PATTERNS = ["rm -rf", "sudo ", "chmod ", "chown ", "mkfs", "dd ", "> /dev"]


# ── Helpers ──────────────────────────────────────────────────────────────────

def log(msg: str):
    ts = time.strftime("%H:%M:%S", time.gmtime())
    print(f"[{ts}] {msg}", flush=True)


def api_call(url: str, payload: dict, timeout: int = 120) -> dict:
    data = json.dumps(payload).encode()
    req = Request(url, data=data, headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read())
    except (URLError, TimeoutError) as e:
        return {"error": str(e)}


def validate_command(cmd: str, sandbox: str) -> str | None:
    for pattern in BLOCKED_PATTERNS:
        if pattern in cmd:
            return f"BLOCKED: dangerous pattern '{pattern}'"
    if sandbox == "read-only":
        if not any(cmd.startswith(p) for p in ALLOWED_RO_PREFIXES):
            return f"BLOCKED: read-only sandbox (not in allowlist)"
    return None


def execute_tool(name: str, args: dict, sandbox: str, workspace: str) -> str:
    if name == "read_file":
        path = args.get("path", "")
        try:
            lines = Path(path).read_text().splitlines()[:200]
            return "\n".join(lines)
        except Exception as e:
            return f"Error: {e}"

    elif name == "run_command":
        cmd = args.get("command", "")
        block = validate_command(cmd, sandbox)
        if block:
            return block
        try:
            r = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=30, cwd=workspace)
            out = (r.stdout + r.stderr)[:2000]
            return out if out.strip() else "(empty output)"
        except subprocess.TimeoutExpired:
            return "Error: command timed out (30s)"
        except Exception as e:
            return f"Error: {e}"

    elif name == "write_file":
        if sandbox == "read-only":
            return "BLOCKED: read-only sandbox"
        path = args.get("path", "")
        if not (path.startswith(workspace) or path.startswith("/tmp/")):
            return "BLOCKED: write outside workspace"
        content = args.get("content", "")
        try:
            Path(path).parent.mkdir(parents=True, exist_ok=True)
            Path(path).write_text(content)
            return f"Written: {path} ({len(content)} bytes)"
        except Exception as e:
            return f"Error: {e}"

    elif name == "done":
        return f"DONE:{args.get('summary', 'no summary')}"

    return f"Unknown tool: {name}"


# ── Main ─────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser(description="CogOS Sandbox Agent")
    parser.add_argument("task", help="Task description")
    parser.add_argument("--model", default=os.environ.get("AGENT_MODEL", "gemma4:26b"))
    parser.add_argument("--sandbox", default=os.environ.get("AGENT_SANDBOX", "read-only"),
                        choices=["none", "read-only", "workspace"])
    parser.add_argument("--max-turns", type=int, default=int(os.environ.get("AGENT_MAX_TURNS", "10")))
    parser.add_argument("--workspace", default=os.environ.get("AGENT_WORKSPACE", os.getcwd()))
    parser.add_argument("--ollama", default=os.environ.get("AGENT_OLLAMA", "http://localhost:11434"))
    parser.add_argument("--kernel", default=os.environ.get("AGENT_KERNEL", "http://localhost:6931"))
    args = parser.parse_args()

    inference_url = f"{args.ollama}/v1/chat/completions"

    # Check if kernel is available for foveated context
    use_kernel = False
    try:
        with urlopen(f"{args.kernel}/health", timeout=2) as r:
            if r.status == 200:
                use_kernel = True
    except Exception:
        pass

    log("╔══════════════════════════════════════╗")
    log("║  CogOS Sandbox Agent                ║")
    log(f"║  Model:     {args.model}")
    log(f"║  Sandbox:   {args.sandbox}")
    log(f"║  Max turns: {args.max_turns}")
    log(f"║  Kernel:    {'yes' if use_kernel else 'no (direct Ollama)'}")
    log("╚══════════════════════════════════════╝")
    log(f"Task: {args.task}")
    log("")

    system = (
        f"You are a CogOS background agent in {args.sandbox} sandbox mode.\n"
        f"Workspace: {args.workspace}\n"
        "Use tools to accomplish the task. Call 'done' with a summary when finished.\n"
        "Be efficient — minimize tool calls."
    )

    # Inject foveated context if kernel available
    if use_kernel:
        try:
            ctx_resp = api_call(f"{args.kernel}/v1/context/foveated", {
                "prompt": args.task,
                "iris": {"size": 128000, "used": 5000},
                "profile": "agent",
            }, timeout=10)
            ctx = ctx_resp.get("data", ctx_resp).get("context", "")
            if ctx:
                system += f"\n\n## Workspace Context\n{ctx}"
                log(f"Foveated context injected ({len(ctx)} chars)")
        except Exception:
            pass

    messages = [
        {"role": "system", "content": system},
        {"role": "user", "content": args.task},
    ]

    for turn in range(1, args.max_turns + 1):
        log(f"── Turn {turn}/{args.max_turns} ──")

        resp = api_call(inference_url, {
            "model": args.model,
            "messages": messages,
            "tools": TOOLS,
            "max_tokens": 1024,
        })

        if "error" in resp:
            log(f"ERROR: {resp['error']}")
            break

        msg = resp["choices"][0]["message"]
        finish = resp["choices"][0].get("finish_reason", "")

        if msg.get("content"):
            log(f"Model: {msg['content'][:200]}")

        tool_calls = msg.get("tool_calls", [])
        if not tool_calls:
            log(f"No tool calls (finish={finish})")
            break

        # Add assistant message
        messages.append(msg)

        # Execute tools
        for tc in tool_calls:
            fn = tc["function"]
            name = fn["name"]
            try:
                tool_args = json.loads(fn["arguments"])
            except json.JSONDecodeError:
                tool_args = {}

            log(f"Tool: {name}({json.dumps(tool_args)[:100]})")
            result = execute_tool(name, tool_args, args.sandbox, args.workspace)

            if result.startswith("DONE:"):
                summary = result[5:]
                log("")
                log("═══ Agent Complete ═══")
                log(f"Summary: {summary}")
                log(f"Turns used: {turn}")
                return

            log(f"  → {result[:150]}{'...' if len(result) > 150 else ''}")

            messages.append({
                "role": "tool",
                "tool_call_id": tc["id"],
                "content": result[:2000],
            })

    log(f"Reached max turns ({args.max_turns})")


if __name__ == "__main__":
    main()
