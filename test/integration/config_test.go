//go:build integration

package integration

import (
	"testing"

	"foci/internal/testharness"
)

// TestL2_Config_PerAgentModelOverridesGroupDefault proves that
// `[[agents]].backend_config.model = "X"` reaches the cc-stub spawn args
// even when the global `[groups] powerful = "Y"` resolves to a different
// model. The assertion looks for the per-agent model name in cc-stub's
// recorded invocation flags — if the per-agent override silently lost
// to the group default, the wrong model name would land in --model.
func TestL2_Config_PerAgentModelOverridesGroupDefault(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_PlatformDisplayCascadesToAgentPlatform proves the
// 5-level cascade `[[agents.platforms.display]]` → `[[platforms.display]]`
// → `[defaults.display]` → code default works through real startup. Set
// only `[platforms.display].show_tool_calls = "preview"` and assert the
// runtime effective value on the agent's platform handler matches —
// either via a log line emitted by foci-gw on resolution or by triggering
// a tool call and inspecting the Telegram stub's recorded message shape.
func TestL2_Config_PlatformDisplayCascadesToAgentPlatform(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_PerAgentDisplayBeatsPlatformDisplay proves a per-agent
// override at `[[agents.platforms.display]]` wins over the global
// `[[platforms.display]]` block. Configure both with conflicting values
// and assert the runtime value matches the per-agent one — proves the
// cascade direction is correct, not just that some cascade fires.
func TestL2_Config_PerAgentDisplayBeatsPlatformDisplay(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_PlatformNotifyAppliesWhenAgentUnset proves that a
// `[[platforms.notify]]` value with no per-agent override actually
// reaches the resolved per-agent NotifyConfig at runtime. Drive a
// scenario whose visible effect depends on `startup_notify` or
// `compaction_notify` (e.g. send a startup message vs. not) and assert
// against the Telegram stub's call log.
func TestL2_Config_PlatformNotifyAppliesWhenAgentUnset(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_DefaultsBehaviorAppliedWhenGlobalUnset proves the
// resolution order `agent > [defaults.behavior] > code default` works
// for a global-or-agent field. Set `[defaults.behavior].steer_mode = false`
// with no global or per-agent override, send a mid-turn user message,
// and assert the steer path is NOT taken — proves the defaults section
// actually wires through the cascade rather than being silently dropped.
func TestL2_Config_DefaultsBehaviorAppliedWhenGlobalUnset(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_CCBackendClaudeBinaryFromGlobal proves the
// `[cc_backend].claude_binary` knob lands at the procx.Spawn call. If
// this regresses, foci-gw spawns "claude" on $PATH instead of the test
// stub and the integration test layer collapses. Assertion: send a
// message, then read cc-stub's recorder — the workdir entry confirms
// the binary that ran was the stub configured globally.
func TestL2_Config_CCBackendClaudeBinaryFromGlobal(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_PerAgentClaudeBinaryOverridesGlobal proves the per-agent
// `[[agents]].backend_config.claude_binary` value beats the global
// `[cc_backend].claude_binary`. Mechanism: write two cc-stub binaries
// (one writing to recorder-A, one to recorder-B), set global to A and
// per-agent to B, send a message to that agent, and assert only
// recorder-B got the invocation.
func TestL2_Config_PerAgentClaudeBinaryOverridesGlobal(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_PlatformTelegramSubBlockInheritedWhenNil is the
// regression net for the ApplyDefaults nil-handling fix area (commit
// 209b9ba3 lineage). When a per-agent platform entry omits the
// `[platforms.telegram]` sub-block entirely, the agent must inherit the
// whole block from the global platform — including `api_base`. Without
// the fix, the agent's bot would point at the real Telegram URL instead
// of the test stub and the test would hang. Driving via the harness's
// existing api_base plumbing makes any future regression a hard failure
// at startup.
func TestL2_Config_PlatformTelegramSubBlockInheritedWhenNil(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_SmartDefaultWorkspaceFromAgentID proves that an agent
// configured without `workspace` gets `$HOME/<id>` as the resolved
// workspace, per load.go's convention defaults. Assertion: send a
// message, then check cc-stub's recorder for the invocation workdir —
// it must match the derived path, not "" or the data_dir.
func TestL2_Config_SmartDefaultWorkspaceFromAgentID(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_SmartDefaultPlatformBotFromAgentID proves that an agent
// whose `[[agents.platforms]]` entry omits `bot` gets `bot = <agent ID>`
// applied by ensureAgentPlatform. Mechanism: configure secrets so the
// agent's expected bot token is at `telegram.<agent-id>`, omit `bot`
// from the agent config, and assert the agent's bot registers
// successfully — proven by the long-poll firing and a Telegram update
// reaching cc-stub.
func TestL2_Config_SmartDefaultPlatformBotFromAgentID(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_SmartDefaultAgentNameFromAgentID proves the
// title-cased-ID default for the agent's display Name. Drive a flow
// that surfaces the Name in an outbound Telegram message (e.g. startup
// notify body) and assert the recorded sendMessage body contains the
// title-cased form — proves load.go's runes-based capitalisation runs.
func TestL2_Config_SmartDefaultAgentNameFromAgentID(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_SmartDefaultMemorySourceFromWorkspace proves that an
// agent with no `[[agents.memory.sources]]` and no `[memory.sources]`
// still gets a memory source pointing at `<workspace>/memory`. Drive a
// memory_search tool call via a scripted cc-stub Bash tool_use and
// assert it returns results from a file the test wrote to
// `<workspace>/memory/` — proves the default source got indexed.
func TestL2_Config_SmartDefaultMemorySourceFromWorkspace(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_SecretTemplateResolvedAtExec proves that a
// `{{secret:custom.token}}` template inside an agent's exec command is
// expanded to the secret value before the subprocess runs. Mechanism:
// add `custom.token` to secrets.toml, script cc-stub to run a Bash
// tool_use containing the template (echo the resolved value to a side
// HTTP server), then assert the server saw the literal secret — not
// the unresolved template, not the empty string.
func TestL2_Config_SecretTemplateResolvedAtExec(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_MissingSecretLoggedAtStartup proves that a referenced
// secret with no matching entry in secrets.toml produces a clear
// startup warning, not a silent fallback to "". Drive a config that
// references `{{secret:custom.absent}}` via a tool command or env, then
// assert foci-gw stderr contains a warning naming the missing key —
// proves RequiredSecrets / startup checks actually fire.
func TestL2_Config_MissingSecretLoggedAtStartup(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_UnknownSecretInTemplateFailsResolution proves that
// runtime resolution of a `{{secret:X}}` template returns an error when
// the secret is missing, rather than the template literal or "". Drive
// a scripted exec invocation referencing an unknown key and assert that
// the tool result records the resolution failure — proves secrets
// Resolve() surfaces the error to the caller and foci doesn't ship the
// bare template to the shell.
func TestL2_Config_UnknownSecretInTemplateFailsResolution(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_MalformedTOMLFailsStartup proves that foci-gw refuses
// to start on a syntactically invalid foci.toml — exits non-zero with a
// parse error message naming the file. Mechanism: write a config with
// an unterminated string or stray bracket, spawn foci-gw via the
// harness, and assert the process exits before the "started N agent(s)"
// ready line with a parse-error in stderr.
func TestL2_Config_MalformedTOMLFailsStartup(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_InvalidDurationFailsValidation proves that a config
// with an unparseable Go duration (e.g. `compaction_threshold = "5xyz"`
// where a duration is required) is rejected by cfg.Validate() at load
// time. Assertion: foci-gw exits non-zero before ready, stderr names
// the field path. Catches regressions where a field is added without
// being wired through validateDurations.
func TestL2_Config_InvalidDurationFailsValidation(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_UnknownTopLevelKeyWarnsNotFails proves that an
// unrecognised top-level TOML key produces a warning log but does NOT
// block startup. cfg.UndefinedKeys is meant to be a soft signal. Drive
// a foci.toml with `mysteryfield = 42` and assert foci-gw reaches ready
// AND stderr contains a warning naming the key.
func TestL2_Config_UnknownTopLevelKeyWarnsNotFails(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_SecretsAllowedAndDeniedAgentsConflictFails proves the
// docs-promised invariant: a secrets.toml section cannot list both
// `allowed_agents` and `denied_agents`. Drive a secrets file with both
// set on the same section and assert foci-gw refuses to start with an
// error naming the section.
func TestL2_Config_SecretsAllowedAndDeniedAgentsConflictFails(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_BoolStringOnOffNormalised proves that the
// normalizeBoolStrings preprocessor accepts `enabled = "on"` /
// `enabled = "off"` as aliases for native booleans on the keys in the
// boolKeys allow-list. Drive `[keepalive] enabled = "on"` and assert
// the keepalive subsystem starts (a keepalive timer log line or a
// keepalive-tagged user message in the recorder).
func TestL2_Config_BoolStringOnOffNormalised(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_GroupsPowerfulModelReachesBackend proves that
// `[groups] powerful = "X"` (where X is a key in [models.*]) resolves
// to the underlying model spec and reaches cc-stub's --model flag. The
// assertion checks the recorder's invocation entry for the model name
// configured under [models.X] — proves group→model resolution actually
// fires during agent startup, not just at backend.SelectModel time.
func TestL2_Config_GroupsPowerfulModelReachesBackend(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_GroupsFastDefaultsToPowerful proves the
// extractGroupNames fallback: when `[groups] powerful = "X"` is set but
// `fast` and `cheap` are omitted, both default to powerful's value.
// Drive a flow that triggers a fast-tier call (e.g. spawn-raw via the
// spawn tool) and assert the resolved model in the recorder matches
// powerful's model — proves the default-on-load logic runs.
func TestL2_Config_GroupsFastDefaultsToPowerful(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_AccessAllowedUsersOnlyTrueRejectsUnlisted proves the
// access cascade: `[platforms.access] allowed_users_only = true` with
// a populated `allowed_users` list blocks messages from other user IDs.
// Send a Telegram update from an unlisted user and assert cc-stub is
// NOT invoked AND a denial log line appears in stderr — proves the
// access check sits in front of agent dispatch, not buried in the loop.
func TestL2_Config_AccessAllowedUsersOnlyTrueRejectsUnlisted(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_PerAgentBotSecretOverrideUsesNamedKey proves the
// `bot_secret` field overrides the default `<platform>.<bot>` secret
// lookup convention. Set `bot_secret = "custom.weird_token"` on the
// agent's platform entry, register that key in secrets.toml, and
// assert the agent's bot long-poll runs against the corresponding
// stub-registered token — proves the override path resolves before
// the convention path.
func TestL2_Config_PerAgentBotSecretOverrideUsesNamedKey(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}

// TestL2_Config_AccessAllowedUsersOnlyFalseAcceptsAny proves the
// inverse access path: with `allowed_users_only = false` and an empty
// `allowed_users` list, any user ID is accepted. Drive a message from
// an arbitrary user and assert cc-stub is invoked normally. Belt-and-
// braces companion to the rejection test; together they pin both
// branches of the access gate.
func TestL2_Config_AccessAllowedUsersOnlyFalseAcceptsAny(t *testing.T) {
	_ = testharness.HarnessOptions{}
	t.Skip("not yet implemented")
}
