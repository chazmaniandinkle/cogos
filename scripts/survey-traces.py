#!/usr/bin/env python3
"""Survey Claude Code session traces — understand the data before building anything.

Karpathy rule: look at the data first. This script answers:
1. How much data do we have?
2. What does an exchange look like?
3. What tools are used and how often?
4. What files are accessed?
5. What does accept/redirect/correct look like in the raw data?
6. What's the distribution of session lengths, tool densities, etc.?

No training, no models. Just understanding.
"""

import json
import os
import sys
from collections import Counter, defaultdict
from pathlib import Path

CLAUDE_DIR = Path.home() / ".claude" / "projects"
COG_WORKSPACE = Path.home() / "workspaces" / "cog"

# ── Parse a single session JSONL ─────────────────────────────────────────────

def parse_session(path: Path) -> dict:
    """Parse a Claude Code session JSONL into structured data."""
    messages = []
    try:
        for line in path.read_text().split("\n"):
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue

            msg_type = entry.get("type", "")
            if msg_type in ("user", "assistant"):
                messages.append(entry)
    except Exception:
        return {"path": str(path), "error": "read_failed", "messages": []}

    return {
        "path": str(path),
        "n_messages": len(messages),
        "messages": messages,
    }


def extract_tool_calls(msg: dict) -> list[dict]:
    """Extract tool calls from an assistant message."""
    tools = []
    message = msg.get("message", msg)
    content = message.get("content", "")

    if isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and block.get("type") == "tool_use":
                tool = {
                    "name": block.get("name", "unknown"),
                    "id": block.get("id", ""),
                    "input": block.get("input", {}),
                }
                # Extract file paths from common tools
                inp = tool["input"]
                if isinstance(inp, dict):
                    tool["file_path"] = (
                        inp.get("file_path") or
                        inp.get("path") or
                        inp.get("command", "")[:200]
                    )
                    tool["pattern"] = inp.get("pattern", "")
                tools.append(tool)
    return tools


def extract_tool_results(msg: dict) -> list[dict]:
    """Extract tool results from a user message (which contains tool_result blocks)."""
    results = []
    message = msg.get("message", msg)
    content = message.get("content", "")

    if isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and block.get("type") == "tool_result":
                result = {
                    "tool_use_id": block.get("tool_use_id", ""),
                    "is_error": block.get("is_error", False),
                    "content_len": len(str(block.get("content", ""))),
                }
                results.append(result)
    return results


def is_real_user_message(msg: dict) -> bool:
    """Distinguish real user input from tool results and meta messages."""
    if msg.get("type") != "user":
        return False
    message = msg.get("message", msg)
    content = message.get("content", "")

    # If content is a list with tool_result blocks, it's a tool result, not user input
    if isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and block.get("type") == "tool_result":
                return False
        # Could be a text block from user
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text":
                return True
        return False

    # String content = real user message
    return isinstance(content, str) and len(content.strip()) > 0


def extract_exchanges(session: dict) -> list[dict]:
    """Extract user→agent exchanges with tool chains and outcomes."""
    messages = session["messages"]
    exchanges = []
    current_exchange = None

    for msg in messages:
        if is_real_user_message(msg):
            # Save previous exchange
            if current_exchange and current_exchange["agent_tools"]:
                exchanges.append(current_exchange)

            message = msg.get("message", msg)
            content = message.get("content", "")
            if isinstance(content, list):
                content = " ".join(
                    b.get("text", "") for b in content
                    if isinstance(b, dict) and b.get("type") == "text"
                )

            current_exchange = {
                "user_query": content[:500],
                "timestamp": msg.get("timestamp", ""),
                "agent_tools": [],
                "files_read": [],
                "files_edited": [],
                "files_searched": [],
                "search_patterns": [],
                "commands_run": [],
                "tool_errors": 0,
                "n_agent_turns": 0,
            }

        elif msg.get("type") == "assistant" and current_exchange is not None:
            current_exchange["n_agent_turns"] += 1
            tools = extract_tool_calls(msg)
            for t in tools:
                current_exchange["agent_tools"].append(t["name"])
                fp = t.get("file_path", "")

                if t["name"] == "Read" and fp:
                    current_exchange["files_read"].append(fp)
                elif t["name"] == "Edit" and fp:
                    current_exchange["files_edited"].append(fp)
                elif t["name"] == "Write" and fp:
                    current_exchange["files_edited"].append(fp)
                elif t["name"] == "Grep":
                    current_exchange["files_searched"].append(fp)
                    if t.get("pattern"):
                        current_exchange["search_patterns"].append(t["pattern"])
                elif t["name"] == "Glob":
                    current_exchange["files_searched"].append(t.get("pattern", ""))
                elif t["name"] == "Bash":
                    current_exchange["commands_run"].append(fp[:200])

        elif msg.get("type") == "user" and current_exchange is not None:
            # Tool results
            results = extract_tool_results(msg)
            for r in results:
                if r["is_error"]:
                    current_exchange["tool_errors"] += 1

    # Don't forget the last exchange
    if current_exchange and current_exchange["agent_tools"]:
        exchanges.append(current_exchange)

    return exchanges


# ── Outcome Detection ────────────────────────────────────────────────────────

def detect_outcome(exchanges: list[dict], idx: int) -> str:
    """Heuristic: what happened after this exchange?

    Returns: 'accept', 'redirect', 'correct', 'retry', 'unknown'
    """
    if idx >= len(exchanges) - 1:
        return "last"  # Can't tell — it's the final exchange

    this_ex = exchanges[idx]
    next_ex = exchanges[idx + 1]
    next_query = next_ex["user_query"].lower()

    # Correction signals
    correction_words = ["no", "wrong", "that's not", "don't", "stop", "undo",
                        "revert", "actually", "i meant", "not what i"]
    if any(w in next_query for w in correction_words):
        return "correct"

    # Redirect signals — user changes topic or asks something different
    if len(next_query) > 50 and not any(
        w in next_query for w in ["yes", "good", "thanks", "perfect", "great", "ok"]
    ):
        return "redirect"

    # Accept signals
    accept_words = ["yes", "good", "thanks", "perfect", "great", "looks good",
                    "nice", "exactly", "that works"]
    if any(w in next_query for w in accept_words):
        return "accept"

    return "unknown"


# ── Main Survey ──────────────────────────────────────────────────────────────

def main():
    # Find all project directories
    if not CLAUDE_DIR.exists():
        print(f"Claude dir not found: {CLAUDE_DIR}")
        sys.exit(1)

    # Find the cog-workspace project dir
    project_dirs = []
    for d in CLAUDE_DIR.iterdir():
        if d.is_dir() and ("cog" in d.name.lower() or "workspaces" in d.name.lower()):
            project_dirs.append(d)

    if not project_dirs:
        # Fall back to all project dirs
        project_dirs = [d for d in CLAUDE_DIR.iterdir() if d.is_dir()]

    print(f"Found {len(project_dirs)} project directories")

    # Collect all session files
    session_files = []
    for pd in project_dirs:
        for f in pd.glob("*.jsonl"):
            if f.stat().st_size > 100:  # Skip empty/tiny files
                session_files.append(f)

    session_files.sort(key=lambda f: f.stat().st_mtime, reverse=True)
    print(f"Found {len(session_files)} session files")
    total_bytes = sum(f.stat().st_size for f in session_files)
    print(f"Total size: {total_bytes / 1024 / 1024:.1f} MB")
    print()

    # Parse sessions (sample if too many)
    max_sessions = int(os.environ.get("MAX_SESSIONS", "200"))
    sample = session_files[:max_sessions]
    print(f"Parsing {len(sample)} sessions...")

    all_exchanges = []
    tool_counter = Counter()
    file_counter = Counter()
    search_counter = Counter()
    outcome_counter = Counter()
    session_lengths = []
    exchange_tool_counts = []

    for sf in sample:
        session = parse_session(sf)
        if session.get("error") or session["n_messages"] < 2:
            continue

        exchanges = extract_exchanges(session)
        session_lengths.append(len(exchanges))

        for i, ex in enumerate(exchanges):
            # Count tools
            for t in ex["agent_tools"]:
                tool_counter[t] += 1

            # Count files accessed
            for f in ex["files_read"]:
                # Normalize to workspace-relative
                f_rel = f.replace(str(COG_WORKSPACE) + "/", "")
                file_counter[f_rel] += 1

            for f in ex["files_searched"]:
                search_counter[f[:100]] += 1

            # Tool density per exchange
            exchange_tool_counts.append(len(ex["agent_tools"]))

            # Detect outcome
            outcome = detect_outcome(exchanges, i)
            outcome_counter[outcome] += 1
            ex["outcome"] = outcome

            all_exchanges.append(ex)

    # ── Report ───────────────────────────────────────────────────────────

    print(f"\n{'='*60}")
    print(f"CLAUDE CODE SESSION TRACE SURVEY")
    print(f"{'='*60}")
    print(f"Sessions parsed:      {len(sample)}")
    print(f"Total exchanges:      {len(all_exchanges)}")
    print(f"Avg exchanges/session: {sum(session_lengths)/max(len(session_lengths),1):.1f}")
    print(f"Max exchanges/session: {max(session_lengths) if session_lengths else 0}")
    print()

    print("── Tool Usage ──")
    for tool, count in tool_counter.most_common(20):
        print(f"  {tool:25s} {count:6d}")
    print()

    print("── Tools per Exchange ──")
    if exchange_tool_counts:
        avg_tools = sum(exchange_tool_counts) / len(exchange_tool_counts)
        print(f"  Mean:   {avg_tools:.1f}")
        print(f"  Median: {sorted(exchange_tool_counts)[len(exchange_tool_counts)//2]}")
        print(f"  Max:    {max(exchange_tool_counts)}")
        # Distribution
        buckets = Counter()
        for c in exchange_tool_counts:
            if c <= 1:
                buckets["0-1"] += 1
            elif c <= 5:
                buckets["2-5"] += 1
            elif c <= 10:
                buckets["6-10"] += 1
            elif c <= 20:
                buckets["11-20"] += 1
            else:
                buckets["21+"] += 1
        print("  Distribution:")
        for b in ["0-1", "2-5", "6-10", "11-20", "21+"]:
            if b in buckets:
                pct = buckets[b] / len(exchange_tool_counts) * 100
                print(f"    {b:8s} {buckets[b]:5d} ({pct:.0f}%)")
    print()

    print("── Outcome Detection ──")
    total_outcomes = sum(outcome_counter.values())
    for outcome, count in outcome_counter.most_common():
        pct = count / total_outcomes * 100 if total_outcomes else 0
        print(f"  {outcome:12s} {count:6d} ({pct:.1f}%)")
    print()

    print("── Top 20 Files Read ──")
    for fp, count in file_counter.most_common(20):
        print(f"  {count:4d}  {fp[:80]}")
    print()

    print("── Top 15 Search Patterns ──")
    for pat, count in search_counter.most_common(15):
        print(f"  {count:4d}  {pat[:80]}")
    print()

    # ── Sample Exchanges ─────────────────────────────────────────────────

    print("── Sample Exchanges (showing 5) ──")
    # Pick diverse samples: one accept, one correct, one with many tools
    samples = []
    for outcome in ["accept", "correct", "redirect"]:
        for ex in all_exchanges:
            if ex["outcome"] == outcome and len(ex["agent_tools"]) >= 3:
                samples.append(ex)
                break
    # Add a high-tool-count example
    by_tools = sorted(all_exchanges, key=lambda e: len(e["agent_tools"]), reverse=True)
    if by_tools:
        samples.append(by_tools[0])
    # Add a retry example (multiple searches for similar things)
    for ex in all_exchanges:
        if len(ex["files_searched"]) >= 3:
            samples.append(ex)
            break

    for i, ex in enumerate(samples[:5]):
        print(f"\n  --- Exchange {i+1} (outcome={ex['outcome']}) ---")
        print(f"  Query: {ex['user_query'][:120]}...")
        print(f"  Tools ({len(ex['agent_tools'])}): {', '.join(ex['agent_tools'][:10])}")
        if ex["files_read"]:
            print(f"  Read: {ex['files_read'][:3]}")
        if ex["files_searched"]:
            print(f"  Searched: {ex['files_searched'][:3]}")
        if ex["files_edited"]:
            print(f"  Edited: {ex['files_edited'][:3]}")
        if ex["tool_errors"]:
            print(f"  Errors: {ex['tool_errors']}")
    print()

    # ── Training Signal Assessment ───────────────────────────────────────

    print("── Training Signal Assessment ──")
    n_with_reads = sum(1 for e in all_exchanges if e["files_read"])
    n_with_searches = sum(1 for e in all_exchanges if e["files_searched"])
    n_with_edits = sum(1 for e in all_exchanges if e["files_edited"])
    n_with_outcome = sum(1 for e in all_exchanges if e["outcome"] in ("accept", "correct", "redirect"))
    n_cogdoc_reads = sum(1 for e in all_exchanges if any(".cog/" in f for f in e["files_read"]))

    print(f"  Exchanges with file reads:      {n_with_reads:5d} ({n_with_reads/max(len(all_exchanges),1)*100:.0f}%)")
    print(f"  Exchanges with searches:         {n_with_searches:5d} ({n_with_searches/max(len(all_exchanges),1)*100:.0f}%)")
    print(f"  Exchanges with edits:            {n_with_edits:5d} ({n_with_edits/max(len(all_exchanges),1)*100:.0f}%)")
    print(f"  Exchanges with CogDoc reads:     {n_cogdoc_reads:5d} ({n_cogdoc_reads/max(len(all_exchanges),1)*100:.0f}%)")
    print(f"  Exchanges with detectable outcome: {n_with_outcome:5d} ({n_with_outcome/max(len(all_exchanges),1)*100:.0f}%)")
    print()

    # Potential training pairs
    n_good_pairs = sum(1 for e in all_exchanges
                       if e["files_read"] and e["outcome"] in ("accept", "last"))
    n_hard_negatives = sum(1 for e in all_exchanges
                          if e["files_read"] and e["outcome"] == "correct")
    print(f"  Potential positive pairs (read + accept): {n_good_pairs}")
    print(f"  Potential hard negatives (read + correct): {n_hard_negatives}")
    print(f"  Potential retry signals (3+ searches):     {sum(1 for e in all_exchanges if len(e['files_searched']) >= 3)}")


if __name__ == "__main__":
    main()
