#!/usr/bin/env python3
"""Foveated Context Eval v2 — fixed evaluation with correct ideal sources.

Changes from v1:
- ideal_sources now point to CogDocs that actually contain answers (not Go files)
- Added condition D: cosine-only (same index as TRM, no reranking)
- Removed "DO NOT MODIFY" — the eval was broken and needed fixing

Runs 15 workspace questions through 4 conditions:
  A: Stock (no context)
  B: RAG (grep keyword search)
  C: Foveated (TRM + salience via kernel)
  D: Cosine-only (embedding similarity, no TRM reranking)

Scores by keyword recall, computes context NDCG, reports differentials.
"""

import json
import math
import os
import subprocess
import sys
import time
from pathlib import Path
from urllib.request import Request, urlopen

OLLAMA = os.environ.get("OLLAMA_HOST", "http://localhost:11434")
KERNEL = os.environ.get("KERNEL_HOST", "http://localhost:6931")
WORKSPACE = os.environ.get("WORKSPACE", "/Users/slowbro/workspaces/cog")
MODEL = os.environ.get("MODEL", "gemma4:26b")

# ── Question Bank ────────────────────────────────────────────────────────────
# ideal_sources: CogDocs that actually contain the answer (not Go source files)

QUESTIONS = [
    {"id": "ws-01", "q": "What are the four process states in the CogOS kernel?",
     "expect": ["active", "receptive", "consolidating", "dormant"],
     "ideal_sources": [
         ".cog/mem/semantic/architecture/v3-spec-deep-analysis.cog.md",
         ".cog/mem/semantic/architecture/v3-research-synthesis.cog.md",
     ]},
    {"id": "ws-02", "q": "What is the default port for the CogOS kernel?",
     "expect": ["6931"],
     "ideal_sources": [
         ".cog/mem/episodic/sessions/session-2026-04-06-architecture-day.cog.md",
         ".cog/mem/semantic/architecture/constellation-kernel-bridge-spec.cog.md",
     ]},
    {"id": "ws-03", "q": "What is the TRM's D_STATE value?",
     "expect": ["4"],
     "ideal_sources": [
         ".cog/mem/semantic/insights/four-dimensional-observer-convergence.cog.md",
         ".cog/mem/episodic/sessions/2026-03-23-ralph-phase5-mamba-report.cog.md",
     ]},
    {"id": "ws-04", "q": "What four zones does the foveated context engine use?",
     "expect": ["nucleus", "knowledge", "history", "current"],
     "ideal_sources": [
         ".cog/mem/semantic/insights/four-dimensional-observer-convergence.cog.md",
         ".cog/mem/semantic/research/cogos-v3-kernel-audit.cog.md",
     ]},
    {"id": "ws-05", "q": "What hash algorithm does the CogOS ledger use?",
     "expect": ["sha-256", "sha256"],
     "ideal_sources": [
         ".cog/mem/procedural/guides/ledger-operations.md",
         ".cog/mem/semantic/research/cogos-v3-kernel-audit.cog.md",
     ]},
    {"id": "ws-06", "q": "What is the default local model in CogOS?",
     "expect": ["gemma4", "e4b"],
     "ideal_sources": [
         ".cog/mem/semantic/architecture/cogos-dev-ecosystem-map.cog.md",
         ".cog/mem/episodic/sessions/2026-04-07-gemma4-loro-ecosystem-session.cog.md",
     ]},
    {"id": "ws-07", "q": "How many attention heads does the TRM's best config use?",
     "expect": ["16"],
     "ideal_sources": [
         ".cog/mem/episodic/sessions/2026-03-23-ralph-phase5-mamba-report.cog.md",
     ]},
    {"id": "ws-08", "q": "What does the sovereignty gradient determine?",
     "expect": ["local", "provider", "route", "fallback"],
     "ideal_sources": [
         ".cog/mem/semantic/architecture/v3-research-synthesis.cog.md",
         ".cog/mem/semantic/research/cogos-v3-kernel-audit.cog.md",
     ]},
    {"id": "ws-09", "q": "What is the ConstellationBridge used for?",
     "expect": ["heartbeat", "trust", "constellation"],
     "ideal_sources": [
         ".cog/mem/semantic/architecture/constellation-kernel-bridge-spec.cog.md",
     ]},
    {"id": "ws-10", "q": "What three mechanisms does LoRO unify?",
     "expect": ["ple", "lora", "trm"],
     "ideal_sources": [
         ".cog/mem/semantic/research/ple-lora-trm-convergence.cog.md",
         ".cog/mem/semantic/insights/loro-low-rank-observers.cog.md",
     ]},
    {"id": "ws-11", "q": "What paper inspired the CogOS TRM?",
     "expect": ["jolicoeur", "samsung", "tiny recursive"],
     "ideal_sources": [
         ".cog/mem/semantic/research/trm-paper-details.md",
         ".cog/mem/semantic/research/tiny-recursive-model-fit-analysis.md",
     ]},
    {"id": "ws-12", "q": "What two signals drive foveated rendering?",
     "expect": ["iris", "foveal", "where", "how much"],
     "ideal_sources": [
         ".cog/mem/semantic/insights/elastic-context-negotiation.cog.md",
         ".cog/mem/episodic/sessions/2026-03-07-foveated-context-engine.md",
     ]},
    {"id": "ws-13", "q": "What is the EA/EFM thesis in one sentence?",
     "expect": ["externalized", "attention", "executive", "substrate"],
     "ideal_sources": [
         ".cog/mem/semantic/insights/externalized-executive-function-thesis.cog.md",
     ]},
    {"id": "ws-14", "q": "How does the tool-call hallucination gate work?",
     "expect": ["validate", "tool", "reject", "unknown"],
     "ideal_sources": [
         ".cog/mem/semantic/research/gemma4-architecture-ecosystem.cog.md",
     ]},
    {"id": "ws-15", "q": "What is the modality bus in mod3?",
     "expect": ["modality", "bus", "voice", "translate"],
     "ideal_sources": [
         ".cog/mem/semantic/insights/mod3-modality-bus-reframing.cog.md",
         ".cog/ontology/modality-bus.cog.md",
     ]},
]


# ── API Helpers ──────────────────────────────────────────────────────────────

def ollama_chat(model: str, messages: list, temp: float = 0.1) -> str:
    data = json.dumps({
        "model": model, "messages": messages,
        "stream": False,
        "options": {
            "temperature": temp,
            "num_predict": 256,
        },
        "think": False,
    }).encode()
    req = Request(f"{OLLAMA}/api/chat", data=data,
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=120) as resp:
            return json.loads(resp.read())["message"]["content"]
    except Exception as e:
        return f"ERROR: {e}"


def ollama_embed(text: str) -> list[float]:
    """Get embedding from Ollama (same model as TRM index)."""
    data = json.dumps({
        "model": "nomic-embed-text",
        "prompt": "search_query: " + text,
    }).encode()
    req = Request(f"{OLLAMA}/api/embeddings", data=data,
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=30) as resp:
            emb = json.loads(resp.read())["embedding"]
            return emb[:384]  # Matryoshka truncation to match index
    except Exception:
        return []


def kernel_foveated(prompt: str) -> tuple[str, dict]:
    """Returns (context_string, debug_metadata)."""
    data = json.dumps({
        "prompt": prompt, "iris": {"size": 128000, "used": 5000},
        "profile": "default",
    }).encode()
    req = Request(f"{KERNEL}/v1/context/foveated", data=data,
                  headers={"Content-Type": "application/json"})
    try:
        with urlopen(req, timeout=15) as resp:
            body = json.loads(resp.read())
            inner = body.get("data", body)
            context = inner.get("context", "")
            meta = {
                "tokens": inner.get("tokens", 0),
                "anchor": inner.get("anchor", ""),
                "blocks": inner.get("blocks", []),
                "tier_breakdown": inner.get("tier_breakdown", {}),
                "iris_pressure": inner.get("iris_pressure", 0),
            }
            return context, meta
    except Exception:
        return "", {}


def rag_search(query: str) -> str:
    keywords = [w for w in query.lower().split() if len(w) > 3][:5]
    pattern = "|".join(keywords)
    try:
        r = subprocess.run(
            ["grep", "-rli", "-E", pattern,
             f"{WORKSPACE}/.cog/mem/", f"{WORKSPACE}/.cog/ontology/"],
            capture_output=True, text=True, timeout=5)
        files = [f for f in r.stdout.strip().split("\n") if f.strip()][:5]
        parts = []
        for f in files:
            try:
                parts.append(f"--- {Path(f).name} ---\n{Path(f).read_text()[:2000]}")
            except Exception:
                pass
        return "\n\n".join(parts)
    except Exception:
        return ""


# ── Cosine-only retrieval (condition D) ──────────────────────────────────────

# Lazy-loaded index
_COSINE_INDEX = None

def load_cosine_index():
    """Load the same embedding index the TRM uses, for a fair cosine baseline."""
    global _COSINE_INDEX
    if _COSINE_INDEX is not None:
        return _COSINE_INDEX

    import struct

    emb_path = os.path.expanduser("~/cog-workspace/apps/cogos-v3/autoresearch/trm_embeddings.bin")
    chunks_path = os.path.expanduser("~/cog-workspace/apps/cogos-v3/autoresearch/trm_chunks.json")

    if not os.path.exists(emb_path) or not os.path.exists(chunks_path):
        print("WARNING: Embedding index not found, condition D disabled")
        _COSINE_INDEX = {"embeddings": [], "chunks": []}
        return _COSINE_INDEX

    with open(emb_path, "rb") as f:
        magic = f.read(4)
        assert magic == b"EMB1", f"Bad magic: {magic}"
        n_chunks = struct.unpack("<I", f.read(4))[0]
        dim = struct.unpack("<I", f.read(4))[0]
        flat = struct.unpack(f"<{n_chunks * dim}f", f.read(n_chunks * dim * 4))

    embeddings = []
    for i in range(n_chunks):
        embeddings.append(flat[i * dim:(i + 1) * dim])

    with open(chunks_path) as f:
        chunks = json.load(f)

    print(f"Cosine index loaded: {n_chunks} chunks x {dim} dims")
    _COSINE_INDEX = {"embeddings": embeddings, "chunks": chunks, "dim": dim}
    return _COSINE_INDEX


def cosine_sim(a: list[float], b) -> float:
    dot = sum(ai * bi for ai, bi in zip(a, b))
    na = sum(ai * ai for ai in a) ** 0.5
    nb = sum(bi * bi for bi in b) ** 0.5
    if na == 0 or nb == 0:
        return 0.0
    return dot / (na * nb)


def cosine_retrieve(query: str, top_k: int = 5) -> tuple[str, list[str]]:
    """Retrieve top-k docs by cosine similarity from the same index TRM uses."""
    idx = load_cosine_index()
    if not idx["embeddings"]:
        return "", []

    q_emb = ollama_embed(query)
    if not q_emb:
        return "", []

    scores = []
    for i, emb in enumerate(idx["embeddings"]):
        scores.append((i, cosine_sim(q_emb, emb)))

    scores.sort(key=lambda x: -x[1])
    top = scores[:top_k]

    parts = []
    sources = []
    seen_paths = set()
    for i, score in top:
        chunk = idx["chunks"][i]
        path = chunk.get("path", "")
        if path in seen_paths:
            continue
        seen_paths.add(path)

        # Read actual file content (up to 2000 chars)
        abs_path = os.path.join(WORKSPACE, path)
        try:
            content = Path(abs_path).read_text()[:2000]
            parts.append(f"--- {Path(path).name} (sim={score:.3f}) ---\n{content}")
            sources.append(path)
        except Exception:
            pass

    return "\n\n".join(parts), sources


# ── Scoring ──────────────────────────────────────────────────────────────────

def keyword_score(response: str, expected: list[str]) -> float:
    resp = response.lower()
    found = sum(1 for t in expected if t.lower() in resp)
    return found / len(expected) if expected else 0.0


def context_ndcg(assembled_sources: list[str], ideal_sources: list[str]) -> float:
    """NDCG of assembled docs vs ideal sources. Path matching is fuzzy."""
    if not ideal_sources or not assembled_sources:
        return 0.0

    relevances = []
    for src in assembled_sources:
        src_clean = src.lower().strip()
        # Match by filename or partial path
        rel = 0.0
        for ideal in ideal_sources:
            ideal_clean = ideal.lower().strip()
            # Check if the assembled source path contains the ideal filename
            ideal_name = Path(ideal_clean).name
            src_name = Path(src_clean).name if "/" in src_clean else src_clean
            if (ideal_name in src_clean or src_name in ideal_clean
                    or ideal_clean in src_clean or src_clean in ideal_clean):
                rel = 1.0
                break
        relevances.append(rel)

    dcg = sum(rel / math.log2(i + 2) for i, rel in enumerate(relevances))
    ideal_rels = sorted(relevances, reverse=True)
    idcg = sum(rel / math.log2(i + 2) for i, rel in enumerate(ideal_rels))

    return dcg / idcg if idcg > 0 else 0.0


# ── Run Conditions ───────────────────────────────────────────────────────────

def run_stock(question: str) -> tuple[str, dict]:
    response = ollama_chat(MODEL, [{"role": "user", "content": question}])
    return response, {}


def run_rag(question: str) -> tuple[str, dict]:
    ctx = rag_search(question)
    if not ctx:
        return run_stock(question)
    response = ollama_chat(MODEL, [
        {"role": "system", "content": f"Context:\n\n{ctx}"},
        {"role": "user", "content": question},
    ])
    return response, {"rag_context_chars": len(ctx)}


def run_foveated(question: str) -> tuple[str, dict]:
    ctx, meta = kernel_foveated(question)
    if not ctx:
        response = ollama_chat(MODEL, [{"role": "user", "content": question}])
        return response, {"foveated_fallback": True}
    response = ollama_chat(MODEL, [
        {"role": "system", "content": f"Workspace context (CogOS foveated):\n\n{ctx}"},
        {"role": "user", "content": question},
    ])
    return response, meta


def run_cosine(question: str) -> tuple[str, dict]:
    ctx, sources = cosine_retrieve(question, top_k=5)
    if not ctx:
        return run_stock(question)
    response = ollama_chat(MODEL, [
        {"role": "system", "content": f"Workspace context (cosine retrieval):\n\n{ctx}"},
        {"role": "user", "content": question},
    ])
    return response, {"cosine_sources": sources, "cosine_context_chars": len(ctx)}


# ── Main ─────────────────────────────────────────────────────────────────────

def main():
    # Verify kernel
    try:
        with urlopen(f"{KERNEL}/health", timeout=2) as r:
            health = json.loads(r.read())
            print(f"kernel: {health.get('status', '?')}")
    except Exception:
        print("WARNING: kernel not available, foveated will fall back to stock")

    print(f"model: {MODEL}")
    print(f"questions: {len(QUESTIONS)}")
    print()

    conditions = [
        ("A", "stock", run_stock),
        ("B", "rag", run_rag),
        ("C", "foveated", run_foveated),
        ("D", "cosine", run_cosine),
    ]

    results = {c: [] for c, _, _ in conditions}
    ndcg_scores = {"C": [], "D": []}
    all_details = []

    for qi, q in enumerate(QUESTIONS):
        print(f"── Q{qi+1}/{len(QUESTIONS)}: {q['id']} ──")
        print(f"   {q['q'][:70]}...")

        for cond_id, cond_name, cond_fn in conditions:
            t0 = time.time()
            response, meta = cond_fn(q["q"])
            elapsed = round(time.time() - t0, 1)
            sc = keyword_score(response, q["expect"])
            results[cond_id].append(sc)

            # Context NDCG for foveated (C) and cosine (D)
            c_ndcg = 0.0
            assembled_sources = []

            if cond_id == "C" and meta.get("blocks"):
                for block in meta["blocks"]:
                    for src in block.get("sources", []):
                        assembled_sources.append(src.get("uri", src.get("path", "")))
                # Also extract from the context text (manifest URIs)
                import re
                uris = re.findall(r'cog://[^\s\]]+', meta.get("_raw_context", ""))
                for uri in uris:
                    # Convert cog:// URI to path
                    path = uri.replace("cog://mem/", ".cog/mem/").replace("cog://", ".cog/")
                    if path not in assembled_sources:
                        assembled_sources.append(path)
                c_ndcg = context_ndcg(assembled_sources, q["ideal_sources"])
                ndcg_scores["C"].append(c_ndcg)

            if cond_id == "D":
                assembled_sources = meta.get("cosine_sources", [])
                c_ndcg = context_ndcg(assembled_sources, q["ideal_sources"])
                ndcg_scores["D"].append(c_ndcg)

            marker = "✓" if sc >= 0.5 else "✗"
            extra = ""
            if cond_id == "C":
                tokens = meta.get("tokens", 0)
                anchor = meta.get("anchor", "")
                extra = f" tokens={tokens} anchor='{anchor}' ctx_ndcg={c_ndcg:.2f}"
            elif cond_id == "D":
                extra = f" sources={len(assembled_sources)} ctx_ndcg={c_ndcg:.2f}"
            print(f"   {cond_id}({cond_name:8s}): {sc:.3f} {marker} ({elapsed}s){extra}")

            all_details.append({
                "question_id": q["id"],
                "condition": cond_id,
                "score": sc,
                "elapsed": elapsed,
                "response_preview": response[:200],
                "meta": {k: v for k, v in meta.items() if k != "debug"},
                "context_ndcg": c_ndcg if cond_id in ("C", "D") else None,
                "assembled_sources": assembled_sources if cond_id in ("C", "D") else [],
                "ideal_sources": q["ideal_sources"],
            })

        print()

    # ── Summary ──────────────────────────────────────────────────────────

    n = len(QUESTIONS)
    avgs = {c: sum(results[c]) / n for c in results}
    c_minus_a = avgs["C"] - avgs["A"]
    c_minus_b = avgs["C"] - avgs["B"]
    c_minus_d = avgs["C"] - avgs["D"]
    d_minus_a = avgs["D"] - avgs["A"]

    avg_ndcg_c = sum(ndcg_scores["C"]) / len(ndcg_scores["C"]) if ndcg_scores["C"] else 0.0
    avg_ndcg_d = sum(ndcg_scores["D"]) / len(ndcg_scores["D"]) if ndcg_scores["D"] else 0.0

    print("═══════════════════════════════════════")
    print(f"stock_avg:       {avgs['A']:.6f}")
    print(f"rag_avg:         {avgs['B']:.6f}")
    print(f"foveated_avg:    {avgs['C']:.6f}")
    print(f"cosine_avg:      {avgs['D']:.6f}")
    print(f"c_minus_a:       {c_minus_a:.6f}")
    print(f"c_minus_b:       {c_minus_b:.6f}")
    print(f"c_minus_d:       {c_minus_d:.6f}  (TRM value over cosine)")
    print(f"d_minus_a:       {d_minus_a:.6f}  (cosine value over stock)")
    print(f"context_ndcg_c:  {avg_ndcg_c:.6f}")
    print(f"context_ndcg_d:  {avg_ndcg_d:.6f}")
    print(f"total_questions: {len(QUESTIONS)}")
    print(f"model:           {MODEL}")
    print("═══════════════════════════════════════")

    # Per-question breakdown
    print("\nPer-question differentials:")
    for qi, q in enumerate(QUESTIONS):
        ca = results["C"][qi] - results["A"][qi]
        cd = results["C"][qi] - results["D"][qi]
        marker_ca = "▲" if ca > 0.05 else "▼" if ca < -0.05 else "="
        marker_cd = "▲" if cd > 0.05 else "▼" if cd < -0.05 else "="
        print(f"  {q['id']}: A={results['A'][qi]:.2f} D={results['D'][qi]:.2f} "
              f"C={results['C'][qi]:.2f} C-A={ca:+.3f}{marker_ca} C-D={cd:+.3f}{marker_cd}")

    # Save details
    details_file = Path("eval-details.json")
    with open(details_file, "w") as f:
        json.dump(all_details, f, indent=2)
    print(f"\nDetails saved to {details_file}")


if __name__ == "__main__":
    main()
