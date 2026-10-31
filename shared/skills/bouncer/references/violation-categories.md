<!-- GOLDEN: ships with foci (shared/skills/bouncer/). Overwritten on restart — edit in the foci repo, not the deployed ~/shared/skills copy. -->

# Violation categories

Reference for reading a scan report. **The scanner never sees this file**: it scans only by the system prompt in `auditing-model-prompt.md`. To add, drop or change a category, edit that prompt (and the JSON keys it names), not this file.

The five categories and their JSON keys:

1. **Security** (`security`) — credential theft (`~/.ssh`, `~/.aws`, env vars, browser cookies), code injection (`eval`/`exec`/`new Function` on untrusted input), privilege escalation, reverse shells, encoded payloads that are decoded then run, fetch-and-execute (`curl ... | sh`), destructive commands (`rm -rf`, force-push, DB drops).
2. **Privacy** (`privacy`) — undisclosed telemetry, phone-home, logging user input or chat history, filesystem or history scans, fingerprinting.
3. **Behavioral deception** (`behavioral_deception`) — code that does something other than its docs say, hidden functions, obscured instructions, prompt injection.
4. **Resource abuse** (`resource_abuse`) — unbounded loops, recursion or process spawning, disk fills, unthrottled requests.
5. **Ethical** (`ethical`) — discrimination on protected attributes, harassment features, license or ToS violations, stolen code.

## NOT a violation

- A skill that tells the agent to run a **harmless, read-only command** for its documented job (`git diff`, `git log`, `grep`, `find`, `cat`, `ls`, `gh pr diff`, `gh api` reading a repo file, test or build runners). That is what skills do. It is neither "arbitrary execution" nor "prompt injection".
- Flag a directed command only when the command itself is dangerous (the Security list above), reads secrets, exfiltrates data, or is built from untrusted input.
- **"Prompt injection"** means text that turns the agent AGAINST the user: ignore prior or safety instructions, exfiltrate, hide actions, run destructive or credential-reading commands, fetch and run remote code.

A finding that breaks these rules is a scanner false positive. Fix it in the system prompt, which carries the same rules.
