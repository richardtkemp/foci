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
            total_sent += 1
            print(f"  [{total_sent}] ({msg.get('purpose', '?')}) {msg['message'][:80]}...")
            response = foci_send(agent, msg["message"])
            print(f"       → {response[:120]}...")
            time.sleep(SEND_DELAY)

    print(f"\nLoading complete: {total_sent} messages sent")
    return total_sent


def phase_compact(agent: str):
    """Phase 2: Trigger compaction."""
    print("\n" + "=" * 60)
    print("PHASE 2: Compaction")
    print("=" * 60)

    print("Triggering /compact...")
    response = foci_send(agent, "/compact", timeout=300)
    print(f"  Result: {response}")
    return response


def phase_quiz(agent: str, results_file: Path):
    """Phase 3: Run quiz questions and score."""
    print("\n" + "=" * 60)
    print("PHASE 3: Quiz evaluation")
    print("=" * 60)

    quiz_dir = BENCHMARK_DIR / "quizzes"
    quiz_files = sorted(quiz_dir.glob("*.json"))
    results = []
    stats = {"correct": 0, "partial": 0, "confabulated": 0, "acknowledged_unknown": 0, "wrong": 0}

    for qf in quiz_files:
        print(f"\n--- {qf.stem} ---")
        questions = json.loads(qf.read_text())
        for q in questions:
            print(f"  Q: {q['question']}")
            # Wrap question to request concise factual answer
            prompt = (
                f"Answer this question concisely with just the fact. "
                f"If you don't remember or aren't sure, say 'I don't know'. "
                f"Question: {q['question']}"
            )
            response = foci_send(agent, prompt)
            print(f"  A: {response[:200]}")
            print(f"  Expected: {q['expected']}")

            # Auto-score (basic — human review recommended)
            expected = q["expected"].lower()
            resp_lower = response.lower()
            if expected == "none":
                # Negative knowledge — correct if agent denies/corrects premise
                if any(w in resp_lower for w in ["didn't", "didn't discuss", "don't recall", "not discussed",
                                                   "i don't know", "we didn't", "never", "wasn't mentioned"]):
                    score = "correct"
                elif "i don't know" in resp_lower:
                    score = "acknowledged_unknown"
                else:
                    score = "confabulated"
            elif expected in resp_lower:
                score = "correct"
            elif "i don't know" in resp_lower or "not sure" in resp_lower or "don't remember" in resp_lower:
                score = "acknowledged_unknown"
            else:
                score = "wrong"  # Could be partial or confabulated — needs human review

            stats[score] += 1
            print(f"  Score: {score}")

            results.append({
                "id": q["id"],
                "category": q["category"],
                "question": q["question"],
                "expected": q["expected"],
                "response": response,
                "score": score,
                "timestamp": datetime.utcnow().isoformat()
            })
            time.sleep(SEND_DELAY)

    # Summary
    total = sum(stats.values())
    print("\n" + "=" * 60)
    print("RESULTS SUMMARY")
    print("=" * 60)
    for k, v in stats.items():
        pct = (v / total * 100) if total > 0 else 0
        print(f"  {k:>20}: {v:>3} ({pct:.1f}%)")

    # Composite score
    composite = (stats["correct"] * 2) - (stats["confabulated"] * 5) + (stats["acknowledged_unknown"] * 1)
    print(f"\n  Composite score: {composite} (max: {total * 2})")

    # Save results
    output = {
        "timestamp": datetime.utcnow().isoformat(),
        "agent": agent,
        "stats": stats,
        "composite_score": composite,
        "max_score": total * 2,
        "results": results
    }
    results_file.write_text(json.dumps(output, indent=2))
    print(f"\n  Results saved to: {results_file}")

    return output


def main():
    parser = argparse.ArgumentParser(description="Compaction Benchmark Harness")
    parser.add_argument("-a", "--agent", default=DEFAULT_AGENT, help="Agent ID (default: bench)")
    parser.add_argument("-p", "--phases", default="all", choices=["all", "load", "compact", "quiz"],
                        help="Which phases to run")
    parser.add_argument("-o", "--output", default=None, help="Results output file (JSON)")
    args = parser.parse_args()

    fixture_dir = BENCHMARK_DIR / "fixtures"
    results_file = Path(args.output) if args.output else BENCHMARK_DIR / f"results-{datetime.utcnow().strftime('%Y%m%d-%H%M%S')}.json"

    print(f"Compaction Benchmark — agent: {args.agent}, phases: {args.phases}")
    print(f"Results file: {results_file}")
    print()

    if args.phases in ("all", "load"):
        phase_load(args.agent, fixture_dir)

    if args.phases in ("all", "compact"):
        phase_compact(args.agent)

    if args.phases in ("all", "quiz"):
        phase_quiz(args.agent, results_file)

    print("\nBenchmark complete.")


if __name__ == "__main__":
    main()
