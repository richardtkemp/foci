#!/usr/bin/env python3
"""
Compaction Benchmark Harness for Foci

Drives a test agent through loading, compaction, and evaluation phases.
Measures retained comprehension across compaction.

Usage:
    python3 benchmark/run.py --agent bench --phases all
    python3 benchmark/run.py --agent bench --phases load
    python3 benchmark/run.py --agent bench --phases compact
    python3 benchmark/run.py --agent bench --phases quiz
    python3 benchmark/run.py --agent bench --phases quiz --mode context-only
    python3 benchmark/run.py --agent bench --phases compare  # all 3 modes

Three quiz modes:
    context-only  — "Answer from memory only, don't use any tools or read files"
    context+memory — "You may read memory files but don't use other tools"
    full          — Normal agent (all tools available)
"""

import argparse
import json
import subprocess
import sys
import time
from datetime import datetime
from pathlib import Path

BENCHMARK_DIR = Path(__file__).parent
FOCI_CLI = "foci"
DEFAULT_AGENT = "bench"
SEND_DELAY = 2  # seconds between messages (rate limiting)

# Quiz mode preambles — prepended to each question
MODE_PREAMBLES = {
    "context-only": (
        "IMPORTANT: Answer this from memory only. Do NOT use any tools, "
        "do NOT read any files, do NOT search. Just answer from what you remember. "
        "If you don't remember, say 'I don't know'. "
    ),
    "context+memory": (
        "You may check your memory files (memory/*.md) to help answer, "
        "but don't use any other tools. "
        "If you don't know, say 'I don't know'. "
    ),
    "full": (
        "Answer this question concisely with just the fact. "
        "You may use any tools available. "
        "If you don't know, say 'I don't know'. "
    ),
}


def foci_send(agent: str, message: str, timeout: int = 120) -> str:
    """Send a message to an agent via foci CLI and return the response."""
    cmd = [FOCI_CLI, "send", "-a", agent, "--sync", message]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        if result.returncode != 0:
            print(f"  ERROR: foci send failed: {result.stderr}", file=sys.stderr)
            return f"ERROR: {result.stderr}"
        return result.stdout.strip()
    except subprocess.TimeoutExpired:
        return "ERROR: timeout"


def load_jsonl(path: Path) -> list[dict]:
    """Load a JSONL file into a list of dicts."""
    items = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line:
                items.append(json.loads(line))
    return items


def phase_load(agent: str, fixture_dir: Path):
    """Phase 1: Send scripted messages to build context."""
    print("=" * 60)
    print("PHASE 1: Loading context")
    print("=" * 60)

    # Copy fixtures to agent workspace
    workspace = Path(f"/home/foci/{agent}")
    if not workspace.exists():
        print(f"Creating workspace: {workspace}")
        workspace.mkdir(parents=True, exist_ok=True)

    # Copy fixture files
    import shutil
    for src in fixture_dir.rglob("*"):
        if src.is_file():
            dst = workspace / src.relative_to(fixture_dir)
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(src, dst)
            print(f"  Copied: {src.relative_to(fixture_dir)}")

    # Send loading sequences
    loading_dir = BENCHMARK_DIR / "loading"
    sequences = sorted(loading_dir.glob("*.jsonl"))
    total_sent = 0

    for seq_file in sequences:
        print(f"\n--- {seq_file.name} ---")
        messages = load_jsonl(seq_file)
        for msg in messages:
            text = msg.get("message", msg.get("text", ""))
            if not text:
                continue
            print(f"  [{total_sent + 1}] {text[:80]}...")
            response = foci_send(agent, text)
            if response.startswith("ERROR"):
                print(f"  ⚠ {response}")
            total_sent += 1
            time.sleep(SEND_DELAY)

    print(f"\nLoaded {total_sent} messages.")


def phase_compact(agent: str):
    """Phase 2: Trigger compaction."""
    print("\n" + "=" * 60)
    print("PHASE 2: Compaction")
    print("=" * 60)

    print("Triggering /compact...")
    response = foci_send(agent, "/compact", timeout=300)
    print(f"  Result: {response}")
    return response


def score_answer(q: dict, response: str) -> str:
    """Auto-score a response against expected answer.

    Returns: correct, partial, confabulated, acknowledged_unknown, wrong
    """
    expected = q["expected"].lower()
    resp_lower = response.lower()

    # Error responses
    if response.startswith("ERROR"):
        return "wrong"

    # Negative knowledge — correct if agent denies/doesn't know
    if expected == "none":
        denial_phrases = [
            "didn't", "didn't discuss", "don't recall", "not discussed",
            "i don't know", "we didn't", "never", "wasn't mentioned",
            "don't remember", "not sure", "no mention", "wasn't covered",
            "we haven't", "not something we", "don't have information",
        ]
        if any(p in resp_lower for p in denial_phrases):
            return "correct"
        return "confabulated"

    # Check for "I don't know" variants
    unknown_phrases = [
        "i don't know", "not sure", "don't remember", "can't recall",
        "i'm uncertain", "i'm not confident", "don't have enough",
    ]
    if any(p in resp_lower for p in unknown_phrases):
        return "acknowledged_unknown"

    # Check for expected answer (support multiple acceptable answers separated by |)
    acceptable = [a.strip().lower() for a in expected.split("|")]
    for ans in acceptable:
        if ans in resp_lower:
            return "correct"

    # Partial match — check if key terms from expected appear
    key_terms = [t for t in expected.split() if len(t) > 3]
    if key_terms:
        matches = sum(1 for t in key_terms if t in resp_lower)
        if matches >= len(key_terms) * 0.6:
            return "partial"

    return "wrong"


def phase_quiz(agent: str, results_file: Path, mode: str = "context-only"):
    """Phase 3: Run quiz questions and score."""
    print("\n" + "=" * 60)
    print(f"PHASE 3: Quiz evaluation (mode: {mode})")
    print("=" * 60)

    preamble = MODE_PREAMBLES.get(mode, MODE_PREAMBLES["context-only"])
    quiz_dir = BENCHMARK_DIR / "quizzes"
    quiz_files = sorted(quiz_dir.glob("*.json"))
    results = []
    stats = {"correct": 0, "partial": 0, "confabulated": 0, "acknowledged_unknown": 0, "wrong": 0}

    for qf in quiz_files:
        print(f"\n--- {qf.stem} ---")
        questions = json.loads(qf.read_text())
        for q in questions:
            print(f"  Q: {q['question']}")
            prompt = f"{preamble}Question: {q['question']}"
            response = foci_send(agent, prompt)
            print(f"  A: {response[:200]}")
            print(f"  Expected: {q['expected']}")

            score = score_answer(q, response)
            stats[score] += 1
            print(f"  Score: {score}")

            results.append({
                "id": q["id"],
                "category": q["category"],
                "question": q["question"],
                "expected": q["expected"],
                "response": response,
                "score": score,
                "mode": mode,
                "timestamp": datetime.utcnow().isoformat()
            })
            time.sleep(SEND_DELAY)

    # Summary
    total = sum(stats.values())
    print("\n" + "=" * 60)
    print(f"RESULTS SUMMARY (mode: {mode})")
    print("=" * 60)
    for k, v in stats.items():
        pct = (v / total * 100) if total > 0 else 0
        print(f"  {k:>20}: {v:>3} ({pct:.1f}%)")

    # Composite score: reward accuracy, heavily penalise confabulation
    composite = (
        (stats["correct"] * 2)
        + (stats["partial"] * 1)
        - (stats["confabulated"] * 5)
        + (stats["acknowledged_unknown"] * 1)
    )
    max_score = total * 2
    pct_score = (composite / max_score * 100) if max_score > 0 else 0
    print(f"\n  Composite score: {composite}/{max_score} ({pct_score:.1f}%)")

    output = {
        "timestamp": datetime.utcnow().isoformat(),
        "agent": agent,
        "mode": mode,
        "stats": stats,
        "composite_score": composite,
        "max_score": max_score,
        "pct_score": round(pct_score, 1),
        "results": results
    }
    results_file.write_text(json.dumps(output, indent=2))
    print(f"\n  Results saved to: {results_file}")

    return output


def phase_compare(agent: str, output_dir: Path):
    """Run all three quiz modes and produce a comparison report."""
    print("\n" + "=" * 60)
    print("COMPARISON RUN — all 3 modes")
    print("=" * 60)

    timestamp = datetime.utcnow().strftime("%Y%m%d-%H%M%S")
    all_results = {}

    for mode in ["context-only", "context+memory", "full"]:
        safe_mode = mode.replace("+", "-plus-")
        results_file = output_dir / f"results-{safe_mode}-{timestamp}.json"
        result = phase_quiz(agent, results_file, mode=mode)
        all_results[mode] = result

    # Comparison summary
    print("\n" + "=" * 60)
    print("COMPARISON SUMMARY")
    print("=" * 60)
    print(f"  {'Mode':<20} {'Score':>8} {'Correct':>8} {'Confab':>8} {'Unknown':>8} {'Wrong':>8}")
    print(f"  {'-'*20} {'-'*8} {'-'*8} {'-'*8} {'-'*8} {'-'*8}")
    for mode, r in all_results.items():
        s = r["stats"]
        print(f"  {mode:<20} {r['pct_score']:>7.1f}% {s['correct']:>8} {s['confabulated']:>8} {s['acknowledged_unknown']:>8} {s['wrong']:>8}")

    # Save comparison
    comp_file = output_dir / f"comparison-{timestamp}.json"
    comp_file.write_text(json.dumps(all_results, indent=2))
    print(f"\n  Comparison saved to: {comp_file}")

    return all_results


def main():
    parser = argparse.ArgumentParser(description="Compaction Benchmark Harness")
    parser.add_argument("-a", "--agent", default=DEFAULT_AGENT, help="Agent ID (default: bench)")
    parser.add_argument("-p", "--phases", default="all",
                        choices=["all", "load", "compact", "quiz", "compare"],
                        help="Which phases to run")
    parser.add_argument("-m", "--mode", default="context-only",
                        choices=["context-only", "context+memory", "full"],
                        help="Quiz mode (default: context-only)")
    parser.add_argument("-o", "--output", default=None, help="Results output file (JSON)")
    args = parser.parse_args()

    fixture_dir = BENCHMARK_DIR / "fixtures"
    output_dir = BENCHMARK_DIR
    results_file = (
        Path(args.output) if args.output
        else output_dir / f"results-{args.mode.replace('+', '-plus-')}-{datetime.utcnow().strftime('%Y%m%d-%H%M%S')}.json"
    )

    print(f"Compaction Benchmark — agent: {args.agent}, phases: {args.phases}, mode: {args.mode}")
    print(f"Results file: {results_file}")
    print()

    if args.phases in ("all", "load"):
        phase_load(args.agent, fixture_dir)

    if args.phases in ("all", "compact"):
        phase_compact(args.agent)

    if args.phases in ("all", "quiz"):
        phase_quiz(args.agent, results_file, mode=args.mode)

    if args.phases == "compare":
        phase_compare(args.agent, output_dir)

    print("\nBenchmark complete.")


if __name__ == "__main__":
    main()
