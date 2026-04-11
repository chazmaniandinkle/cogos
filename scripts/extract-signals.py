#!/usr/bin/env python3
"""Extract Training Signals — idempotent, source-agnostic signal extraction.

Reads Claude Code session traces, extracts (query, docs, outcome) triples,
and writes them to a content-addressed signal store. Re-running on the same
data produces the same result — safe to bulk-import entire history.

Sources:
  - Raw JSONL from ~/.claude/projects/ (native parser)
  - claude-devtools parsed JSON (TODO: MCP bridge)
  - Attention signals from .cog/run/attention.jsonl

Output:
  training-signals/
    index.json          — signal ID → metadata (dedup key)
    signals/            — one .json per signal, named by content hash
      ab3f1c2e.json
      ...

Content-addressing: signal_id = sha256(session_id + exchange_index)[:8]
Idempotent: if signal_id exists, skip. Re-import entire history safely.
"""

import hashlib
import json
import os
import sys
from collections import Counter
from datetime import datetime
from pathlib import Path

# ── Config ───────────────────────────────────────────────────────────────────

WORKSPACE = Path(os.environ.get("WORKSPACE", os.path.expanduser("~/workspaces/cog")))
CLAUDE_DIR = Path.home() / ".claude" / "projects"
SIGNALS_DIR = Path(os.environ.get("SIGNALS_DIR",
    os.path.expanduser("~/cog-workspace/apps/cogos-v3/autoresearch/training-signals")))
INDEX_FILE = SIGNALS_DIR / "index.json"
SIGS_SUBDIR = SIGNALS_DIR / "signals"

# ── Signal ID ────────────────────────────────────────────────────────────────

def signal_id(session_id: str, exchange_idx: int) -> str:
    """Stable content-addressed ID. Same session + same exchange = same ID."""
    raw = f"{session_id}:{exchange_idx}"
    return hashlib.sha256(raw.encode()).hexdigest()[:12]


def session_id_from_path(path: Path) -> str:
    """Extract stable session ID from JSONL filename."""
    return path.stem  # e.g. "fdb3d667-ae34-4dc1-b660-e98fd28f5a99"


# ── Index ────────────────────────────────────────────────────────────────────

def load_index() -> dict:
    if INDEX_FILE.exists():
        return json.loads(INDEX_FILE.read_text())
    return {"version": 2, "signals": {}, "sessions_processed": {}}


def save_index(index: dict):
    SIGNALS_DIR.mkdir(parents=True, exist_ok=True)
    INDEX_FILE.write_text(json.dumps(index, indent=2))


def save_signal(sig: dict):
    SIGS_SUBDIR.mkdir(parents=True, exist_ok=True)
    path = SIGS_SUBDIR / f"{sig['id']}.json"
    path.write_text(json.dumps(sig, indent=2))


# ── Parsing ──────────────────────────────────────────────────────────────────

def is_real_user_message(entry: dict) -> bool:
    if entry.get("type") != "user":
        return False
    message = entry.get("message", entry)
    content = message.get("content", "")
    if isinstance(content, list):
        has_tool_result = any(
            isinstance(b, dict) and b.get("type") == "tool_result"
            for b in content
        )
        if has_tool_result:
            return False
        return any(isinstance(b, dict) and b.get("type") == "text" for b in content)
    return isinstance(content, str) and len(content.strip()) > 0


def extract_text(entry: dict) -> str:
    message = entry.get("message", entry)
    content = message.get("content", "")
    if isinstance(content, str):
        return content.strip()
    if isinstance(content, list):
        parts = []
        for b in content:
            if isinstance(b, dict) and b.get("type") == "text":
                parts.append(b.get("text", ""))
        return " ".join(parts).strip()
    return ""


def extract_tool_calls(entry: dict) -> list[dict]:
    tools = []
    message = entry.get("message", entry)
    content = message.get("content", "")
    if not isinstance(content, list):
        return tools
    for block in content:
        if not isinstance(block, dict) or block.get("type") != "tool_use":
            continue
        inp = block.get("input", {})
        if not isinstance(inp, dict):
            inp = {}
        tools.append({
            "name": block.get("name", ""),
            "id": block.get("id", ""),
            "file_path": inp.get("file_path") or inp.get("path") or "",
            "pattern": inp.get("pattern", ""),
            "command": str(inp.get("command", ""))[:200],
            "old_string": str(inp.get("old_string", ""))[:100],
            "new_string": str(inp.get("new_string", ""))[:100],
        })
    return tools


def has_tool_errors(entry: dict) -> int:
    message = entry.get("message", entry)
    content = message.get("content", "")
    if not isinstance(content, list):
        return 0
    return sum(1 for b in content if isinstance(b, dict) and b.get("is_error"))


def detect_outcome(exchanges: list[dict], idx: int) -> str:
    if idx >= len(exchanges) - 1:
        return "last"
    next_q = exchanges[idx + 1].get("query", "").lower()

    corrections = ["no,", "no ", "wrong", "that's not", "don't", "stop",
                   "undo", "revert", "actually,", "i meant", "not what i",
                   "nope", "that isn't"]
    for w in corrections:
        if next_q.startswith(w) or f" {w}" in next_q[:80]:
            return "correct"

    accepts = ["yes", "good", "thanks", "perfect", "great", "looks good",
               "nice", "exactly", "that works", "awesome", "sweet", "let's"]
    for w in accepts:
        if next_q.startswith(w) or next_q.startswith(w + ",") or next_q.startswith(w + " "):
            return "accept"

    return "continue"


def normalize_path(path: str) -> str:
    """Normalize to workspace-relative path."""
    ws = str(WORKSPACE)
    alt_ws = os.path.expanduser("~/cog-workspace")
    if path.startswith(ws + "/"):
        return os.path.relpath(path, ws)
    if path.startswith(alt_ws + "/"):
        return os.path.relpath(path, alt_ws)
    home = os.path.expanduser("~")
    if path.startswith(home + "/cog-workspace/"):
        return path.replace(home + "/cog-workspace/", "")
    if path.startswith(home + "/workspaces/cog/"):
        return path.replace(home + "/workspaces/cog/", "")
    return path


# ── Session Parser ───────────────────────────────────────────────────────────

def parse_session(path: Path) -> list[dict]:
    """Parse a Claude Code session JSONL into exchanges."""
    entries = []
    try:
        for line in path.read_text(errors="replace").split("\n"):
            line = line.strip()
            if not line:
                continue
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                continue
            if entry.get("type") in ("user", "assistant"):
                entries.append(entry)
    except Exception:
        return []

    # Group into exchanges (user query → agent response chain)
    exchanges = []
    current = None

    for entry in entries:
        if is_real_user_message(entry):
            # Save previous exchange — for cascade detection, keep even if no reads
            if current:
                exchanges.append(current)
            query_text = extract_text(entry)
            current = {
                "query": query_text[:1000],
                "query_hash": hashlib.sha256(query_text[:500].encode()).hexdigest()[:8],
                "timestamp": entry.get("timestamp", ""),
                "reads": [],
                "searches": [],
                "edits": [],
                "tool_chain": [],
                "errors": 0,
                "n_turns": 0,
            }

        elif entry.get("type") == "assistant" and current is not None:
            current["n_turns"] += 1
            for tool in extract_tool_calls(entry):
                name = tool["name"]
                fp = tool["file_path"]
                current["tool_chain"].append(name)

                if name == "Read" and fp:
                    current["reads"].append(normalize_path(fp))
                elif name in ("Edit", "Write") and fp:
                    current["edits"].append(normalize_path(fp))
                elif name in ("Grep", "Glob"):
                    search = tool.get("pattern") or fp
                    if search:
                        current["searches"].append(search[:200])
                elif name == "Agent":
                    current["tool_chain"].append("Agent(subagent)")

        elif entry.get("type") == "user" and current is not None:
            current["errors"] += has_tool_errors(entry)

    if current:
        exchanges.append(current)

    # Filter: for individual signals, require reads. For cascade detection, keep all.
    all_exchanges = exchanges  # keep full list for cascade detection

    # Detect outcomes (on full list, for cascade window detection)
    for i, ex in enumerate(exchanges):
        ex["outcome"] = detect_outcome(exchanges, i)

    return exchanges


# ── Build Signal ─────────────────────────────────────────────────────────────

# ── Cascade Detection ─────────────────────────────────────────────────────────

CASCADE_MARKERS = [
    "wait", "oh", "that's it", "this means", "and then", "which means",
    "so that", "holy", "dude", "exactly", "right?", "think about",
    "what if", "isn't that", "coincidence", "convergence", "emergence",
    "the same", "isomorphi", "it's literally", "eigenform", "that's why",
    "could we", "what about", "and that means", "so basically",
    "that's the", "this is the", "i think", "oh wait",
]


def detect_cascade_windows(exchanges: list[dict]) -> list[dict]:
    """Detect contiguous windows of insight-cascade behavior.

    A cascade window is 3+ consecutive exchanges where:
    - User messages are short (<300 chars) and frequent
    - Cascade marker words are present
    - Tool density is low (thinking, not searching)

    Returns list of cascade windows with their doc context.
    """
    windows = []
    current_window = None

    for i, ex in enumerate(exchanges):
        query = ex.get("query", "")
        q_lower = query.lower()
        q_len = len(query)

        is_cascade_msg = (
            q_len < 400
            and any(m in q_lower for m in CASCADE_MARKERS)
        )

        # Also count rapid short messages without markers as cascade-adjacent
        is_rapid = q_len < 200 and ex.get("n_turns", 0) <= 3

        if is_cascade_msg or (is_rapid and current_window is not None):
            if current_window is None:
                current_window = {
                    "start_idx": i,
                    "exchanges": [],
                    "all_reads": [],
                    "all_edits": [],
                    "all_searches": [],
                    "marker_count": 0,
                    "queries": [],
                }
            current_window["exchanges"].append(i)
            current_window["all_reads"].extend(ex.get("reads", []))
            current_window["all_edits"].extend(ex.get("edits", []))
            current_window["all_searches"].extend(ex.get("searches", []))
            current_window["queries"].append(query[:200])
            if is_cascade_msg:
                current_window["marker_count"] += 1
        else:
            # Break in cascade — save if long enough
            if current_window and len(current_window["exchanges"]) >= 3:
                current_window["end_idx"] = current_window["exchanges"][-1]
                current_window["length"] = len(current_window["exchanges"])
                current_window["density"] = (
                    current_window["marker_count"] / current_window["length"]
                )
                windows.append(current_window)
            current_window = None

    # Don't forget trailing window
    if current_window and len(current_window["exchanges"]) >= 3:
        current_window["end_idx"] = current_window["exchanges"][-1]
        current_window["length"] = len(current_window["exchanges"])
        current_window["density"] = (
            current_window["marker_count"] / current_window["length"]
        )
        windows.append(current_window)

    return windows


# ── Build Signal ─────────────────────────────────────────────────────────────

def exchange_to_signal(exchange: dict, session: str, idx: int) -> dict:
    """Convert an exchange to a training signal."""
    sid = signal_id(session, idx)
    outcome = exchange["outcome"]

    # Partition reads into positives/negatives based on outcome
    reads = list(dict.fromkeys(exchange["reads"]))  # dedupe preserving order

    if outcome in ("accept", "last"):
        positives = reads
        negatives = []
    elif outcome == "correct":
        positives = []
        negatives = reads
    else:
        # "continue" — weak positive (user didn't complain)
        positives = reads
        negatives = []

    return {
        "id": sid,
        "session": session,
        "exchange_idx": idx,
        "query": exchange["query"],
        "query_hash": exchange["query_hash"],
        "positives": positives,
        "negatives": negatives,
        "edits": list(dict.fromkeys(exchange["edits"])),
        "searches": exchange["searches"][:10],
        "tool_chain": exchange["tool_chain"][:30],
        "outcome": outcome,
        "n_turns": exchange["n_turns"],
        "errors": exchange["errors"],
        "timestamp": exchange["timestamp"],
    }


# ── Provenance Scan ──────────────────────────────────────────────────────────

HIGH_VALUE_PREFIXES = [
    ".cog/mem/semantic/insights/",
    ".cog/mem/semantic/architecture/",
    ".cog/mem/semantic/research/",
    ".cog/mem/episodic/decisions/",
    ".cog/mem/episodic/journal/",
    ".cog/mem/procedural/",
    ".cog/mem/reflective/",
    ".cog/adr/",
    ".cog/ontology/",
    ".cog/docs/",
]


def classify_cogdoc(path: str) -> str:
    pl = path.lower()
    if "/insights/" in pl: return "insight"
    if "/architecture/" in pl: return "architecture"
    if "/research/" in pl: return "research"
    if "/adr/" in pl: return "adr"
    if "/decisions/" in pl: return "decision"
    if "/ontology/" in pl: return "ontology"
    if "/procedural/" in pl: return "procedural"
    if "/reflective/" in pl: return "reflective"
    if "/journal/" in pl: return "journal"
    if "/docs/" in pl: return "docs"
    return "other"


def is_high_value(path: str) -> bool:
    return any(p in path for p in HIGH_VALUE_PREFIXES)


def run_provenance(index: dict, reprocess: bool = False):
    """Scan all sessions for CogDoc authorship via tool calls.

    For each session, finds:
    - Every Write/Edit to a high-value CogDoc (the output)
    - Every Read in the same session (the input context)
    Creates 'provenance' signals: reading THESE docs produced THAT doc.
    """
    print("── Provenance scan ──")

    # Scan all sessions, track tool calls in order per exchange
    print("Scanning sessions for Write/Edit→Read proximity...")
    session_count = 0
    new_signals = 0
    from collections import Counter
    cat_counts = Counter()

    for pd in CLAUDE_DIR.iterdir():
        if not pd.is_dir():
            continue
        for f in pd.glob("*.jsonl"):
            if f.stat().st_size < 500:
                continue
            sid = f.stem
            session_count += 1

            # Parse into exchanges (reuse our parser)
            exchanges = parse_session(f)
            if not exchanges:
                continue

            # For each exchange that writes a high-value CogDoc,
            # capture reads from THIS exchange + the 3 preceding exchanges
            for i, ex in enumerate(exchanges):
                edits = ex.get("edits", [])
                cogdoc_writes = [e for e in edits if is_high_value(e) and e.endswith(".md")]
                if not cogdoc_writes:
                    continue

                # Collect reads: this exchange + 3 prior (the "context window")
                nearby_reads = []
                for j in range(max(0, i - 3), i + 1):
                    nearby_reads.extend(exchanges[j].get("reads", []))
                nearby_reads = list(dict.fromkeys(nearby_reads))

                for doc_path in cogdoc_writes:
                    prov_id = signal_id(f"{sid}:provenance:{doc_path}", i)

                    if prov_id in index["signals"] and not reprocess:
                        continue

                    category = classify_cogdoc(doc_path)
                    cat_counts[category] += 1

                    sig = {
                        "id": prov_id,
                        "session": sid,
                        "type": "provenance",
                        "exchange_idx": i,
                        "query": ex.get("query", "")[:1000] or f"[authored: {Path(doc_path).name}]",
                        "query_hash": hashlib.sha256(doc_path.encode()).hexdigest()[:8],
                        "positives": nearby_reads,
                        "negatives": [],
                        "edits": [doc_path],
                        "searches": ex.get("searches", [])[:10],
                        "tool_chain": ex.get("tool_chain", [])[:30],
                        "outcome": "provenance",
                        "write_type": category,
                        "n_turns": ex.get("n_turns", 0),
                        "errors": ex.get("errors", 0),
                        "timestamp": ex.get("timestamp", ""),
                    }

                    save_signal(sig)
                    index["signals"][prov_id] = {
                        "session": sid,
                        "outcome": "provenance",
                        "write_type": category,
                        "n_positives": len(nearby_reads),
                        "n_negatives": 0,
                        "timestamp": sig["timestamp"],
                        "authored_doc": doc_path,
                    }
                    new_signals += 1

    print(f"Sessions scanned: {session_count}")
    print(f"New provenance signals: {new_signals}")
    print(f"By category: {dict(cat_counts)}")

    # Total
    all_outcomes = Counter(m.get("outcome", "?") for m in index["signals"].values())
    print(f"\nComplete signal inventory:")
    for o, c in all_outcomes.most_common():
        print(f"  {o:20s} {c:5d}")
    print(f"  {'TOTAL':20s} {sum(all_outcomes.values()):5d}")


# ── Main ─────────────────────────────────────────────────────────────────────

def main():
    import argparse
    parser = argparse.ArgumentParser(description="Extract training signals (idempotent)")
    parser.add_argument("--source", default="claude",
                        choices=["claude", "attention", "all"],
                        help="Which source to extract from")
    parser.add_argument("--provenance", action="store_true",
                        help="Scan all sessions for CogDoc authorship (read→write chains)")
    parser.add_argument("--reprocess", action="store_true",
                        help="Reprocess already-seen sessions (update signals)")
    parser.add_argument("--stats", action="store_true",
                        help="Show stats without extracting")
    args = parser.parse_args()

    index = load_index()

    if args.provenance:
        run_provenance(index, args.reprocess)
        save_index(index)
        return

    if args.stats:
        n_signals = len(index["signals"])
        n_sessions = len(index["sessions_processed"])
        outcomes = Counter(m.get("outcome", "?") for m in index["signals"].values())
        print(f"Signals: {n_signals}")
        print(f"Sessions processed: {n_sessions}")
        print(f"Outcomes: {dict(outcomes)}")
        print(f"Signal dir: {SIGNALS_DIR}")
        return

    # Find all session files
    session_files = []
    if args.source in ("claude", "all") and CLAUDE_DIR.exists():
        for pd in CLAUDE_DIR.iterdir():
            if not pd.is_dir():
                continue
            for f in pd.glob("*.jsonl"):
                if f.stat().st_size > 500:
                    session_files.append(f)

    session_files.sort(key=lambda f: f.stat().st_mtime)
    print(f"Found {len(session_files)} session files")

    # Filter already-processed (unless --reprocess)
    if not args.reprocess:
        processed = set(index.get("sessions_processed", {}).keys())
        session_files = [f for f in session_files if session_id_from_path(f) not in processed]
        print(f"New sessions: {len(session_files)}")

    new_signals = 0
    skipped = 0
    outcome_counts = Counter()

    for sf in session_files:
        sid = session_id_from_path(sf)
        exchanges = parse_session(sf)

        if not exchanges:
            continue

        # Emit individual exchange signals (only those with reads)
        for i, ex in enumerate(exchanges):
            if not ex.get("reads"):
                continue  # Skip read-less exchanges for individual signals

            sig = exchange_to_signal(ex, sid, i)

            # Idempotent: skip if signal already exists
            if sig["id"] in index["signals"] and not args.reprocess:
                skipped += 1
                continue

            # Store signal
            save_signal(sig)
            index["signals"][sig["id"]] = {
                "session": sid,
                "outcome": sig["outcome"],
                "n_positives": len(sig["positives"]),
                "n_negatives": len(sig["negatives"]),
                "timestamp": sig["timestamp"],
            }
            new_signals += 1
            outcome_counts[sig["outcome"]] += 1

        # Detect cascade windows (uses ALL exchanges, including read-less ones)
        cascades = detect_cascade_windows(exchanges)
        for ci, cw in enumerate(cascades):
            cascade_sid = signal_id(f"{sid}:cascade", ci)
            if cascade_sid in index["signals"] and not args.reprocess:
                skipped += 1
                continue

            # All docs read during the cascade are strong positives —
            # they enabled the flow state
            all_reads = list(dict.fromkeys(cw["all_reads"]))
            # Docs read BEFORE the cascade (context that set it up)
            pre_reads = []
            if cw["start_idx"] > 0:
                for prev_i in range(max(0, cw["start_idx"] - 3), cw["start_idx"]):
                    if prev_i < len(exchanges):
                        pre_reads.extend(exchanges[prev_i].get("reads", []))
            pre_reads = list(dict.fromkeys(pre_reads))

            cascade_sig = {
                "id": cascade_sid,
                "session": sid,
                "type": "cascade",
                "exchange_range": [cw["start_idx"], cw["end_idx"]],
                "length": cw["length"],
                "density": round(cw["density"], 2),
                "query": " → ".join(q[:60] for q in cw["queries"][:5]),
                "query_hash": hashlib.sha256(
                    " ".join(cw["queries"]).encode()
                ).hexdigest()[:8],
                "positives": all_reads + pre_reads,
                "negatives": [],
                "edits": list(dict.fromkeys(cw["all_edits"])),
                "searches": cw["all_searches"][:10],
                "tool_chain": [],
                "outcome": "cascade",
                "n_turns": cw["length"],
                "errors": 0,
                "timestamp": exchanges[cw["start_idx"]].get("timestamp", ""),
            }

            save_signal(cascade_sig)
            index["signals"][cascade_sid] = {
                "session": sid,
                "outcome": "cascade",
                "n_positives": len(cascade_sig["positives"]),
                "n_negatives": 0,
                "timestamp": cascade_sig["timestamp"],
                "cascade_length": cw["length"],
                "cascade_density": round(cw["density"], 2),
            }
            new_signals += 1
            outcome_counts["cascade"] += 1

        # Detect crystallization events (read CogDocs → write new CogDoc)
        for i, ex in enumerate(exchanges):
            edits = ex.get("edits", [])
            reads = ex.get("reads", [])

            cogdoc_writes = [e for e in edits if ".cog/" in e and e.endswith(".md")]
            cogdoc_reads = [r for r in reads if ".cog/" in r]

            if not cogdoc_writes:
                continue

            cryst_id = signal_id(f"{sid}:crystallize", i)
            if cryst_id in index["signals"] and not args.reprocess:
                skipped += 1
                continue

            # Classify the write target
            write_type = "cogdoc"
            for w in cogdoc_writes:
                wl = w.lower()
                if "/insights/" in wl:
                    write_type = "insight"
                elif "/journal/" in wl:
                    write_type = "journal"
                elif "/adr/" in wl or "adr-" in wl:
                    write_type = "adr"
                elif "handoff" in wl:
                    write_type = "handoff"
                elif "/architecture/" in wl:
                    write_type = "architecture"
                elif "/research/" in wl:
                    write_type = "research"

            cryst_sig = {
                "id": cryst_id,
                "session": sid,
                "type": "crystallization",
                "exchange_idx": i,
                "query": ex.get("query", "")[:1000],
                "query_hash": ex.get("query_hash", ""),
                "positives": list(dict.fromkeys(cogdoc_reads + reads)),
                "negatives": [],
                "edits": list(dict.fromkeys(cogdoc_writes)),
                "searches": ex.get("searches", [])[:10],
                "tool_chain": ex.get("tool_chain", [])[:30],
                "outcome": "crystallization",
                "write_type": write_type,
                "n_turns": ex.get("n_turns", 0),
                "errors": ex.get("errors", 0),
                "timestamp": ex.get("timestamp", ""),
            }

            save_signal(cryst_sig)
            index["signals"][cryst_id] = {
                "session": sid,
                "outcome": "crystallization",
                "write_type": write_type,
                "n_positives": len(cryst_sig["positives"]),
                "n_negatives": 0,
                "timestamp": cryst_sig["timestamp"],
            }
            new_signals += 1
            outcome_counts["crystallization"] += 1

        # Mark session as processed
        index["sessions_processed"][sid] = {
            "path": str(sf),
            "exchanges": len(exchanges),
            "cascades": len(cascades),
            "processed_at": datetime.now().isoformat(),
            "mtime": sf.stat().st_mtime,
        }

    save_index(index)

    total = len(index["signals"])
    print(f"\nNew signals: {new_signals} (skipped {skipped} existing)")
    print(f"Total signals: {total}")
    print(f"Sessions processed: {len(index['sessions_processed'])}")
    if outcome_counts:
        print(f"New outcomes: {dict(outcome_counts)}")

    # Summary stats
    all_outcomes = Counter(m.get("outcome", "?") for m in index["signals"].values())
    print(f"\nAll outcomes: {dict(all_outcomes)}")
    n_pos = sum(1 for m in index["signals"].values() if m.get("n_positives", 0) > 0)
    n_neg = sum(1 for m in index["signals"].values() if m.get("n_negatives", 0) > 0)
    print(f"With positives: {n_pos}")
    print(f"With negatives: {n_neg}")


if __name__ == "__main__":
    main()
