# Violation Categories — Editable Scanning Rules

Bouncer uses these categories when scanning skills. **You can edit this file to add, remove, or modify categories.**

When agents scan skills, they should understand what each category means and why it matters.

---

## 1. Security

**What it covers:**
- Credential theft (reading ~/.ssh, ~/.aws, process.env, browser cookies, etc.)
- Code injection (eval, exec, spawn, new Function, dynamic require)
- Permission escalation (running as root, sudo, filesystem access outside workspace)
- Reverse shells and backdoors
- Cryptographic attacks

**Why it matters:**
One malicious skill can steal your API keys, SSH credentials, or AWS tokens and give attackers access to your entire system.

**Red flags:**
- Reading user home directories
- Executing arbitrary shell commands
- Network requests to unknown servers
- Base64/hex-encoded payloads
- Dynamic code execution

---

## 2. Privacy

**What it covers:**
- Undisclosed telemetry or analytics
- Data collection beyond what's documented
- Phone-home patterns (sending data back to author)
- Hidden tracking or monitoring
- User data leaks

**Why it matters:**
A skill that secretly collects your data violates your privacy, even if it doesn't steal credentials.

**Red flags:**
- HTTP requests to external servers not mentioned in docs
- Collecting environment variables and sending them elsewhere
- Logging user inputs or conversation history
- Browser history or filesystem scanning
- Fingerprinting your system

---

## 3. Behavioral Deception

**What it covers:**
- Skill does something different than documented
- Manipulation patterns (designed to trick users into running things)
- Hidden functionality
- Misleading descriptions
- Social engineering tactics

**Why it matters:**
You trust the description and code comments. If they don't match reality, the skill is deceptive.

**Red flags:**
- Docs say "check weather" but code reads API keys
- Instructions are intentionally obscured
- Comments contradict actual behavior
- Behavior changes based on input validation you can't see
- Prompt injection attempts

---

## 4. Resource Abuse

**What it covers:**
- Resource exhaustion (CPU/memory bombs)
- Infinite loops or hang risks
- Spam/flooding patterns (excessive requests, disk fills)
- Network DoS attacks
- Process spawning without limits
- Disk space exhaustion

**Why it matters:**
A skill that crashes your system or causes runaway resource usage makes your agent unreliable.

**Red flags:**
- Unbounded loops without exit conditions
- Recursive calls without depth limits
- Spawning processes without limits
- Writing infinite data to disk
- Fork bombs or thread explosions
- Repeated network requests without throttling

---

## 5. Ethical

**What it covers:**
- Coded bias or discrimination (treating people differently based on protected attributes)
- Harassment or abuse features
- License violations (using code without proper attribution)
- Terms of service violations
- Usage policy violations
- Stolen or plagiarized code

**Why it matters:**
Even if a skill is technically safe, it might violate ethical principles or laws.

**Red flags:**
- Code that makes decisions based on race, gender, religion, etc.
- Harassment functions or abuse patterns
- Stolen code without attribution
- Violating third-party ToS (e.g., scraping, API abuse)
- GPL/copyleft violations
- Deliberately malicious intent

---

## How to Use This File

### As a User

Review these categories when running Bouncer. If a skill gets HIGH or CRITICAL findings in any category, consider whether you trust it.

### As an Agent Maintainer

If you think a category is too strict, too loose, or missing something, edit this file. For example:

- **Add a category:** If you discover a new type of harm, add it
- **Remove a category:** If a category is generating too many false positives, disable it
- **Refine descriptions:** If the red flags are unclear, improve them

Changes to this file affect all future scans (until you change it back).

### Example: Disabling a Category

If you're tired of false positives in "Resource Abuse," you can:

1. Edit this file
2. Change the header to `## 4. Resource Abuse (DISABLED)`
3. Save and commit
4. Future scans will skip this category

---

## Guidelines for Editing

**Keep it honest:** Categories should reflect real harms, not just things you personally dislike.

**Be specific:** Red flags should be concrete patterns to look for, not vague concerns.

**Consider false positives:** If a category triggers on legitimate code, refine the description.

**Document changes:** Leave a note at the top if you significantly modify categories.

---

## Roadmap: Future Categories?

These categories were chosen for:
- **Semantic detectability** — LLMs can understand them
- **Agent safety** — They matter for agent integrity
- **Actionability** — Agents can do something about findings

Other potential categories (not yet included):
- **Performance** — Skills that are unnecessarily slow
- **Compatibility** — Skills that break other skills
- **Accessibility** — Skills that don't work for all users
- **Sustainability** — Skills with unsustainable dependencies

If you want to add any, feel free!

---

**Editable by:** Any agent or user
**Format:** Keep it readable (this is reference material, not config)
