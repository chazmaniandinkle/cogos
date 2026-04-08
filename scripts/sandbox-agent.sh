#!/usr/bin/env bash
# sandbox-agent.sh — Sandboxed background agent using CogOS + Gemma 4 26B
#
# A lightweight tool-use agent loop that:
#   1. Gets foveated workspace context from the CogOS kernel
#   2. Sends prompts to Gemma 4 26B (via kernel or direct Ollama)
#   3. Parses tool calls from the response
#   4. Executes approved tools in a sandbox
#   5. Loops until the model says it's done
#
# The kernel provides: context assembly, tool-call validation, ledger recording.
# The model provides: reasoning and tool-call generation.
# This script provides: the agent loop and sandboxed execution.
#
# Usage:
#   bash scripts/sandbox-agent.sh "Review the recent changes and summarize"
#   bash scripts/sandbox-agent.sh --task-file tasks/daily-review.md
#   AGENT_MODEL=gemma4:e4b bash scripts/sandbox-agent.sh "Quick check"
#
# Environment:
#   AGENT_MODEL      Model to use (default: gemma4:26b)
#   AGENT_WORKSPACE  Workspace root (default: current dir or $HOME/cog-workspace)
#   AGENT_PORT       CogOS kernel port (default: 6931)
#   AGENT_OLLAMA     Ollama endpoint (default: http://localhost:11434)
#   AGENT_MAX_TURNS  Max agent turns (default: 10)
#   AGENT_SANDBOX    Sandbox level: none, read-only, workspace (default: read-only)
#   AGENT_LOG        Log file (default: /tmp/cogos-agent.log)

set -euo pipefail

# ── Config ───────────────────────────────────────────────────────────────────

MODEL="${AGENT_MODEL:-gemma4:26b}"
WORKSPACE="${AGENT_WORKSPACE:-$(pwd)}"
PORT="${AGENT_PORT:-6931}"
OLLAMA="${AGENT_OLLAMA:-http://localhost:11434}"
MAX_TURNS="${AGENT_MAX_TURNS:-10}"
SANDBOX="${AGENT_SANDBOX:-read-only}"
LOG="${AGENT_LOG:-/tmp/cogos-agent.log}"
KERNEL="http://localhost:$PORT"

# Task from argument or file
TASK="${1:-}"
if [ "$TASK" = "--task-file" ] && [ -n "${2:-}" ]; then
    TASK=$(cat "$2")
fi

if [ -z "$TASK" ]; then
    echo "Usage: sandbox-agent.sh \"task description\""
    echo "       sandbox-agent.sh --task-file path/to/task.md"
    exit 1
fi

# ── Helpers ──────────────────────────────────────────────────────────────────

log() {
    echo "[$(date -u +%H:%M:%S)] $*" | tee -a "$LOG"
}

# Check if kernel is available, fall back to direct Ollama
INFERENCE_URL="$KERNEL/v1/chat/completions"
USE_KERNEL=false
if curl -sf "$KERNEL/health" >/dev/null 2>&1; then
    USE_KERNEL=true
    log "Using CogOS kernel at $KERNEL (foveated context + tool validation)"
else
    INFERENCE_URL="$OLLAMA/v1/chat/completions"
    log "CogOS kernel not available, using Ollama directly at $OLLAMA"
fi

# ── Tool Definitions ��────────────────────────────────────────────────────────

TOOLS='[
  {
    "type": "function",
    "function": {
      "name": "read_file",
      "description": "Read the contents of a file",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "File path to read"}
        },
        "required": ["path"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "list_directory",
      "description": "List files in a directory",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "Directory path"},
          "pattern": {"type": "string", "description": "Optional glob pattern"}
        },
        "required": ["path"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "search_files",
      "description": "Search for a pattern in files",
      "parameters": {
        "type": "object",
        "properties": {
          "pattern": {"type": "string", "description": "Search pattern (regex)"},
          "path": {"type": "string", "description": "Directory to search in"}
        },
        "required": ["pattern"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "run_command",
      "description": "Run a shell command (read-only sandbox: only inspection commands allowed)",
      "parameters": {
        "type": "object",
        "properties": {
          "command": {"type": "string", "description": "Shell command to execute"}
        },
        "required": ["command"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "write_file",
      "description": "Write content to a file (only in workspace sandbox mode)",
      "parameters": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "File path to write"},
          "content": {"type": "string", "description": "Content to write"}
        },
        "required": ["path", "content"]
      }
    }
  },
  {
    "type": "function",
    "function": {
      "name": "done",
      "description": "Signal that the task is complete and provide a summary",
      "parameters": {
        "type": "object",
        "properties": {
          "summary": {"type": "string", "description": "Summary of what was accomplished"}
        },
        "required": ["summary"]
      }
    }
  }
]'

# ── Sandbox Enforcement ──────────────────────────────────────────────────────

# Read-only allowed commands (prefix match)
ALLOWED_RO_COMMANDS="cat head tail less wc ls find grep rg git\ log git\ status git\ diff git\ show go\ vet go\ build python3\ -m\ py_compile python3\ -c curl ollama"

validate_command() {
    local cmd="$1"
    if [ "$SANDBOX" = "none" ]; then
        return 0
    fi

    # Block dangerous patterns in all sandbox modes
    if echo "$cmd" | grep -qE 'rm -rf|sudo|chmod|chown|mkfs|dd |curl.*POST|wget.*-O'; then
        echo "BLOCKED: destructive command"
        return 1
    fi

    if [ "$SANDBOX" = "read-only" ]; then
        local allowed=false
        for prefix in $ALLOWED_RO_COMMANDS; do
            if [[ "$cmd" == "$prefix"* ]]; then
                allowed=true
                break
            fi
        done
        if [ "$allowed" = "false" ]; then
            echo "BLOCKED: read-only sandbox (command not in allowlist)"
            return 1
        fi
    fi

    return 0
}

validate_write() {
    local path="$1"
    if [ "$SANDBOX" = "none" ]; then
        return 0
    fi
    if [ "$SANDBOX" = "read-only" ]; then
        echo "BLOCKED: read-only sandbox"
        return 1
    fi
    # workspace mode: only allow writes under workspace
    if [[ "$path" != "$WORKSPACE"* ]] && [[ "$path" != "/tmp/"* ]]; then
        echo "BLOCKED: write outside workspace"
        return 1
    fi
    return 0
}

# ── Tool Execution ───────────────────────────────────────────────────────────

execute_tool() {
    local tool_name="$1"
    local tool_args="$2"

    case "$tool_name" in
        read_file)
            local path=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin)['path'])" 2>/dev/null)
            if [ -f "$path" ]; then
                head -200 "$path"
            else
                echo "Error: file not found: $path"
            fi
            ;;
        list_directory)
            local path=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin)['path'])" 2>/dev/null)
            local pattern=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin).get('pattern',''))" 2>/dev/null)
            if [ -n "$pattern" ]; then
                find "$path" -name "$pattern" -maxdepth 2 2>/dev/null | head -50
            else
                ls -la "$path" 2>/dev/null | head -50
            fi
            ;;
        search_files)
            local pattern=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin)['pattern'])" 2>/dev/null)
            local path=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin).get('path','.'))" 2>/dev/null)
            grep -rn "$pattern" "$path" --include="*.go" --include="*.py" --include="*.ts" --include="*.md" 2>/dev/null | head -30
            ;;
        run_command)
            local cmd=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin)['command'])" 2>/dev/null)
            local validation
            validation=$(validate_command "$cmd")
            if [ $? -ne 0 ]; then
                echo "$validation"
            else
                eval "$cmd" 2>&1 | head -100
            fi
            ;;
        write_file)
            local path=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin)['path'])" 2>/dev/null)
            local content=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin)['content'])" 2>/dev/null)
            local validation
            validation=$(validate_write "$path")
            if [ $? -ne 0 ]; then
                echo "$validation"
            else
                mkdir -p "$(dirname "$path")"
                echo "$content" > "$path"
                echo "Written: $path ($(wc -c < "$path") bytes)"
            fi
            ;;
        done)
            local summary=$(echo "$tool_args" | python3 -c "import sys,json; print(json.load(sys.stdin)['summary'])" 2>/dev/null)
            echo "AGENT_DONE:$summary"
            ;;
        *)
            echo "Unknown tool: $tool_name"
            ;;
    esac
}

# ── Foveated Context ─────────────────────────────────────────────────────────

get_foveated_context() {
    local prompt="$1"
    if [ "$USE_KERNEL" = "true" ]; then
        local ctx
        ctx=$(curl -sf -X POST "$KERNEL/v1/context/foveated" \
            -H "Content-Type: application/json" \
            -d "{\"prompt\":$(echo "$prompt" | python3 -c 'import sys,json; print(json.dumps(sys.stdin.read()))'),\"iris\":{\"size\":128000,\"used\":5000},\"profile\":\"agent\"}" 2>/dev/null)
        if [ -n "$ctx" ]; then
            echo "$ctx" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('data',d).get('context',d.get('context','')))" 2>/dev/null
        fi
    fi
}

# ── Agent Loop ───────────────────────────────────────────────────────────────

log "╔══════════════════════════════════════╗"
log "║  CogOS Sandbox Agent                ║"
log "╠══════════════════════════════════════╣"
log "║  Model:     $MODEL"
log "║  Sandbox:   $SANDBOX"
log "║  Workspace: $WORKSPACE"
log "║  Max turns: $MAX_TURNS"
log "║  Kernel:    $( [ "$USE_KERNEL" = "true" ] && echo "yes ($KERNEL)" || echo "no (direct Ollama)" )"
log "╚══════════════════════════════════════╝"
log ""
log "Task: $TASK"
log ""

# Build system prompt
SYSTEM_PROMPT="You are a CogOS background agent running in a sandboxed environment.
Sandbox level: $SANDBOX
Workspace: $WORKSPACE

You have tools available: read_file, list_directory, search_files, run_command, write_file, done.
Use tools to accomplish the task. Call 'done' with a summary when finished.
Be efficient — minimize unnecessary tool calls. Focus on the task."

# Get foveated context if kernel is available
CONTEXT=""
if [ "$USE_KERNEL" = "true" ]; then
    CONTEXT=$(get_foveated_context "$TASK")
    if [ -n "$CONTEXT" ]; then
        SYSTEM_PROMPT="$SYSTEM_PROMPT

## Workspace Context (from CogOS foveated engine)
$CONTEXT"
        log "Foveated context injected ($(echo "$CONTEXT" | wc -c | tr -d ' ') chars)"
    fi
fi

# Initialize conversation
MESSAGES="[{\"role\":\"system\",\"content\":$(echo "$SYSTEM_PROMPT" | python3 -c 'import sys,json; print(json.dumps(sys.stdin.read()))')},{\"role\":\"user\",\"content\":$(echo "$TASK" | python3 -c 'import sys,json; print(json.dumps(sys.stdin.read()))')}]"

for turn in $(seq 1 "$MAX_TURNS"); do
    log "── Turn $turn/$MAX_TURNS ──"

    # Call the model
    RESPONSE=$(curl -sf -X POST "$INFERENCE_URL" \
        -H "Content-Type: application/json" \
        -d "{
            \"model\": \"$MODEL\",
            \"messages\": $MESSAGES,
            \"tools\": $TOOLS,
            \"max_tokens\": 2048
        }" 2>/dev/null)

    if [ -z "$RESPONSE" ] || echo "$RESPONSE" | grep -q "CURL_FAILED\|error"; then
        log "ERROR: inference failed"
        log "Response: $(echo "$RESPONSE" | head -c 200)"
        break
    fi

    # Parse response
    CONTENT=$(echo "$RESPONSE" | python3 -c "
import sys, json
d = json.load(sys.stdin)
msg = d['choices'][0]['message']
if msg.get('content'):
    print(msg['content'][:500])
" 2>/dev/null || echo "")

    TOOL_CALLS=$(echo "$RESPONSE" | python3 -c "
import sys, json
d = json.load(sys.stdin)
msg = d['choices'][0]['message']
calls = msg.get('tool_calls', [])
for c in calls:
    fn = c['function']
    print(f\"{c['id']}|{fn['name']}|{fn['arguments']}\")
" 2>/dev/null || echo "")

    FINISH=$(echo "$RESPONSE" | python3 -c "
import sys, json
d = json.load(sys.stdin)
print(d['choices'][0].get('finish_reason',''))
" 2>/dev/null || echo "")

    if [ -n "$CONTENT" ]; then
        log "Model: $CONTENT"
    fi

    # If no tool calls, we're done
    if [ -z "$TOOL_CALLS" ]; then
        log "No tool calls, finish_reason=$FINISH"
        break
    fi

    # Add assistant message to conversation
    ASSISTANT_MSG=$(echo "$RESPONSE" | python3 -c "
import sys, json
d = json.load(sys.stdin)
print(json.dumps(d['choices'][0]['message']))
" 2>/dev/null)
    MESSAGES=$(echo "$MESSAGES" | python3 -c "
import sys, json
msgs = json.load(sys.stdin)
msgs.append(json.loads('$ASSISTANT_MSG'))
print(json.dumps(msgs))
" 2>/dev/null)

    # Execute each tool call
    while IFS='|' read -r call_id tool_name tool_args; do
        [ -z "$call_id" ] && continue
        log "Tool: $tool_name($tool_args)"

        RESULT=$(execute_tool "$tool_name" "$tool_args" 2>&1)

        # Check for done signal
        if echo "$RESULT" | grep -q "^AGENT_DONE:"; then
            SUMMARY=$(echo "$RESULT" | sed 's/^AGENT_DONE://')
            log ""
            log "═══ Agent Complete ═══"
            log "Summary: $SUMMARY"
            log ""
            exit 0
        fi

        log "Result: $(echo "$RESULT" | head -3)"
        [ "$(echo "$RESULT" | wc -l)" -gt 3 ] && log "  ... ($(echo "$RESULT" | wc -l) lines total)"

        # Add tool result to conversation
        MESSAGES=$(echo "$MESSAGES" | python3 -c "
import sys, json
msgs = json.load(sys.stdin)
msgs.append({
    'role': 'tool',
    'tool_call_id': '$call_id',
    'content': $(echo "$RESULT" | head -100 | python3 -c 'import sys,json; print(json.dumps(sys.stdin.read()))')
})
print(json.dumps(msgs))
" 2>/dev/null)

    done <<< "$TOOL_CALLS"
done

log ""
log "Agent reached max turns ($MAX_TURNS)"
