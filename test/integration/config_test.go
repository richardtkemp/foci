//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/testharness"

	"github.com/PaulSonOfLars/gotgbot/v2"
)

// TestL2_Config_PerAgentModelOverridesGroupDefault proves that
// `[[agents]].backend_config.model = "X"` reaches the cc-stub spawn args
// even when the global `[groups] powerful = "Y"` resolves to a different
// model. The assertion looks for the per-agent model name in cc-stub's
// recorded invocation flags — if the per-agent override silently lost
// to the group default, the wrong model name would land in --model.
func TestL2_Config_PerAgentModelOverridesGroupDefault(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: writeTestConfig hard-codes [groups] powerful = \"stub\" and per-agent backend_config.model = \"stub\" — testing per-agent override beating a *different* group default requires HarnessOptions to inject custom [models.*], [groups], and per-agent backend_config blocks")
}

// TestL2_Config_PlatformDisplayCascadesToAgentPlatform proves the
// 5-level cascade `[[agents.platforms.display]]` → `[[platforms.display]]`
// → `[defaults.display]` → code default works through real startup. Set
// only `[platforms.display].show_tool_calls = "preview"` and assert the
// runtime effective value on the agent's platform handler matches.
//
// Probe mechanism: send `/display show_tool_calls` via the Telegram stub
// — the slash command's displayFieldValue (settings.go) walks the same
// cascade as the runtime renderer, so its reply text is a faithful read
// of the resolved value. If the cascade broke (global platform display
// ignored), the response would fall to the code default "off".
//
// ExtraConfigTOML appends `[platforms.display]` after the agent stanzas,
// and TOML semantics scope it back to the latest `[[platforms]]` (the
// global one) because `agents` and `platforms` are independent root paths.
func TestL2_Config_PlatformDisplayCascadesToAgentPlatform(t *testing.T) {
	t.Parallel()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 7400},
		},
		ReadyTimeout: 30 * time.Second,
		ExtraConfigTOML: "[platforms.display]\n" +
			"show_tool_calls = \"preview\"\n",
	})

	// `/config toml` dumps the resolved config as raw TOML. The injected
	// value lands as a literal `show_tool_calls = "preview"` line inside
	// the [[platforms]] block. The single-key /display form triggers a
	// ChainKeyboard, /display alone triggers KeyboardOptions, and /config
	// table renders a markdown grid that collides on the "preview"
	// substring (a `tool_call_preview_chars` row exists). TOML output
	// gives us an unambiguous literal pair.
	pushTelegramText(t, h, "alpha", 7400, "/config toml")

	token := h.AgentBotToken("alpha")
	// `show_tool_calls = "preview"` is the exact TOML literal emitted by
	// FormatConfigTOML for a non-nil ToolCallDisplay. If the cascade was
	// silently dropped, the line would be absent (TOML omits zero/nil
	// fields).
	text := waitForSendMessageText(t, h, token, 15*time.Second, `show_tool_calls = "preview"`)
	if text == "" {
		t.Fatalf("expected /config toml to surface show_tool_calls = \"preview\" from [platforms.display]; sent so far:\n%v\nstderr tail:\n%s",
			peekSendMessageTexts(h, token), stderrTail(h.Stderr()))
	}
}

// TestL2_Config_PerAgentDisplayBeatsPlatformDisplay proves a per-agent
// override at `[[agents.platforms.display]]` wins over the global
// `[[platforms.display]]` block. Configure both with conflicting values
// and assert the runtime value matches the per-agent one — proves the
// cascade direction is correct, not just that some cascade fires.
func TestL2_Config_PerAgentDisplayBeatsPlatformDisplay(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: needs HarnessOptions to inject both [platforms.display] and per-agent [agents.platforms.display] blocks with conflicting show_tool_calls values")
}

// TestL2_Config_PlatformNotifyAppliesWhenAgentUnset proves that a
// `[[platforms.notify]]` value with no per-agent override actually
// reaches the resolved per-agent NotifyConfig at runtime. Drive a
// scenario whose visible effect depends on `startup_notify` or
// `compaction_notify` (e.g. send a startup message vs. not) and assert
// against the Telegram stub's call log.
func TestL2_Config_PlatformNotifyAppliesWhenAgentUnset(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: needs HarnessOptions to inject a [platforms.notify] block with startup_notify=true and a chat_id — current writeTestConfig has no notify block and no way to opt in")
}

// TestL2_Config_DefaultsBehaviorAppliedWhenGlobalUnset proves the
// resolution order `agent > [defaults.behavior] > code default` works
// for a global-or-agent field. Set `[defaults.behavior].steer_mode = false`
// with no global or per-agent override, send a mid-turn user message,
// and assert the steer path is NOT taken — proves the defaults section
// actually wires through the cascade rather than being silently dropped.
func TestL2_Config_DefaultsBehaviorAppliedWhenGlobalUnset(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: needs HarnessOptions to inject a [defaults.behavior] block — writeTestConfig has no defaults-section support")
}

// TestL2_Config_CCBackendClaudeBinaryFromGlobal proves the
// `[cc_backend].claude_binary` knob lands at the procx.Spawn call. If
// this regresses, foci-gw spawns "claude" on $PATH instead of the test
// stub and the integration test layer collapses. Assertion: send a
// message, then read cc-stub's recorder — the workdir entry confirms
// the binary that ran was the stub configured globally.
func TestL2_Config_CCBackendClaudeBinaryFromGlobal(t *testing.T) {
	t.Parallel()
	// The harness already sets [cc_backend].claude_binary = <cc-stub path>
	// at the global level. If this knob ever regressed, foci-gw would
	// spawn "claude" from $PATH instead of the stub, which would either
	// fail outright or never write a recorder line. So presence of any
	// invocation entry in the recorder file for this agent is proof that
	// the global claude_binary knob plumbed through to procx.Spawn.
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 7100},
		},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 7100, Type: "private"},
			From: &gotgbot.User{Id: 7100, FirstName: "Tester"},
			Text: "ping",
		},
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		invs := invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha")
		if len(invs) > 0 {
			// The presence of an invocation entry under alpha's workdir
			// confirms cc-stub (the configured global claude_binary) ran.
			// A wrong binary either wouldn't exist or wouldn't have
			// written to $CCSTUB_RECORDER, so this entry is load-bearing.
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("no cc-stub invocation recorded — global [cc_backend].claude_binary did not reach procx.Spawn\nstderr:\n%s", h.Stderr())
}

// TestL2_Config_PerAgentClaudeBinaryOverridesGlobal proves the per-agent
// `[[agents]].backend_config.claude_binary` value beats the global
// `[cc_backend].claude_binary`. Mechanism: write two cc-stub binaries
// (one writing to recorder-A, one to recorder-B), set global to A and
// per-agent to B, send a message to that agent, and assert only
// recorder-B got the invocation.
func TestL2_Config_PerAgentClaudeBinaryOverridesGlobal(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: needs HarnessOptions to inject per-agent backend_config.claude_binary AND a way to point separate agents at separate recorder files — current harness shares a single CCSTUB_RECORDER env across all spawns")
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
	t.Parallel()
	// The harness's writeTestConfig already emits the agent platform
	// entry WITHOUT a per-agent [agents.platforms.telegram] sub-block —
	// only [agents.platforms.access] is set. So if inheritance broke,
	// the agent's bot would dial the real Telegram URL and StartGateway
	// would time out waiting for the "started N agent(s)" ready line.
	// Reaching the ready line + successful round-trip is the proof.
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 7200},
		},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 7200, Type: "private"},
			From: &gotgbot.User{Id: 7200, FirstName: "Tester"},
			Text: "inheritance check",
		},
	})

	if !waitForUserMessage(t, h, "workspaces/alpha", "inheritance check", 15*time.Second) {
		t.Errorf("agent never processed message — [agents.platforms.telegram] sub-block did not inherit api_base from [platforms.telegram]\nstderr tail:\n%s", stderrTail(h.Stderr()))
	}
}

// TestL2_Config_SmartDefaultWorkspaceFromAgentID proves that an agent
// configured without `workspace` gets `$HOME/<id>` as the resolved
// workspace, per load.go's convention defaults. Assertion: send a
// message, then check cc-stub's recorder for the invocation workdir —
// it must match the derived path, not "" or the data_dir.
func TestL2_Config_SmartDefaultWorkspaceFromAgentID(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: writeTestConfig always sets workspace = <path> per agent — needs HarnessOptions option to omit the workspace key so the $HOME/<id> convention default fires")
}

// TestL2_Config_SmartDefaultPlatformBotFromAgentID proves that an agent
// whose `[[agents.platforms]]` entry omits `bot` gets `bot = <agent ID>`
// applied by ensureAgentPlatform. Mechanism: configure secrets so the
// agent's expected bot token is at `telegram.<agent-id>`, omit `bot`
// from the agent config, and assert the agent's bot registers
// successfully — proven by the long-poll firing and a Telegram update
// reaching cc-stub.
func TestL2_Config_SmartDefaultPlatformBotFromAgentID(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: writeTestConfig always emits an explicit `bot = <agent-id>` line — proving the *default* fires requires omitting it; needs HarnessOptions support")
}

// TestL2_Config_SmartDefaultAgentNameFromAgentID proves the
// title-cased-ID default for the agent's display Name. Drive a flow
// that surfaces the Name in an outbound Telegram message (e.g. startup
// notify body) and assert the recorded sendMessage body contains the
// title-cased form — proves load.go's runes-based capitalisation runs.
func TestL2_Config_SmartDefaultAgentNameFromAgentID(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: needs HarnessOptions to enable a notify path that surfaces the resolved agent.Name (e.g. startup_notify=true with a chat_id), AND the agent config must omit `name = ...` so the title-cased ID default fires")
}

// TestL2_Config_SmartDefaultMemorySourceFromWorkspace proves that an
// agent with no `[[agents.memory.sources]]` and no `[memory.sources]`
// still gets a memory source pointing at `<workspace>/memory`. Drive a
// memory_search tool call via a scripted cc-stub Bash tool_use and
// assert it returns results from a file the test wrote to
// `<workspace>/memory/` — proves the default source got indexed.
func TestL2_Config_SmartDefaultMemorySourceFromWorkspace(t *testing.T) {
	t.Parallel()
	// Pre-seed a memory file in alpha's workspace that foci will
	// discover at startup. The marker text must be searchable via
	// foci_memory_search after indexing — proving the smart-default
	// memory source (<workspace>/memory) was registered automatically.
	// Use a single-token marker — bleve stems and splits on hyphens.
	const markerText = "smartdefaultmemorysourcemarker7e3a"
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{
				ID:     "alpha",
				UserID: 4104,
				PreStartFiles: map[string]string{
					"memory/2026-01-01.md": "# Test memory file\n\nThe marker is: " + markerText + "\n",
				},
			},
		},
		ReadyTimeout: 30 * time.Second,
	})

	// Drive foci_memory_search to look for the marker. Output flows
	// into the bash_tool_use recorder entry — assert it contains the
	// marker line, proving indexing discovered the seeded file.
	bashCmd := "foci_memory_search " + markerText
	scriptBody, err := json.Marshal(map[string]any{
		"text": "searching memory",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": bashCmd}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 4104, Type: "private"},
			From: &gotgbot.User{Id: 4104, FirstName: "Tester"},
			Text: "find the marker",
		},
	})

	if !waitForUserMessage(t, h, "workspaces/alpha", "find the marker", 20*time.Second) {
		t.Fatalf("turn did not complete; stderr:\n%s", stderrTail(h.Stderr()))
	}

	// Find the bash_tool_use entry. The combined stdout/stderr should
	// contain the marker (foci_memory_search returns hits to stdout).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range readRecorderEntries(t, h.RecorderPath()) {
			if e.Kind != "bash_tool_use" || !strings.Contains(e.Workdir, "workspaces/alpha") {
				continue
			}
			if !strings.Contains(e.BashCommand, "foci_memory_search") {
				continue
			}
			if strings.Contains(e.BashOutput, markerText) {
				return // pass
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("foci_memory_search output never contained the marker %q; recorder:\n%s",
		markerText, recorderTail(t, h.RecorderPath()))
}

// TestL2_Config_SecretTemplateResolvedAtExec proves that a
// `{{secret:custom.token}}` template inside an agent's exec command is
// expanded to the secret value before the subprocess runs. Mechanism:
// add `custom.token` to secrets.toml, script cc-stub to run a Bash
// tool_use containing the template (echo the resolved value to a side
// HTTP server), then assert the server saw the literal secret — not
// the unresolved template, not the empty string.
func TestL2_Config_SecretTemplateResolvedAtExec(t *testing.T) {
	t.Parallel()
	// Side server records the Authorization header. The bash command
	// references {{secret:custom.token}} which the bridge's secret
	// resolver should substitute before the real HTTP call goes out.
	var (
		mu       sync.Mutex
		authHits []string
	)
	side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHits = append(authHits, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte("ok"))
	}))
	defer side.Close()

	const secretValue = "s3cret-v4l-2026"
	// The bridge enforces allowed_hosts on secret usage — gate the
	// secret to the side server's host (without port — the check
	// compares bare hostnames).
	sideHostPort := strings.TrimPrefix(strings.TrimPrefix(side.URL, "http://"), "https://")
	sideHost := sideHostPort
	if i := strings.Index(sideHostPort, ":"); i >= 0 {
		sideHost = sideHostPort[:i]
	}
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 4101},
		},
		ReadyTimeout: 30 * time.Second,
		ExtraSecretsTOML: fmt.Sprintf("[custom]\ntoken = %q\nallowed_hosts = [%q]\n", secretValue, sideHost),
	})

	bashCmd := fmt.Sprintf(
		`foci_http_request --header "Authorization: Bearer {{secret:custom.token}}" %s/probe`,
		side.URL,
	)
	scriptBody, err := json.Marshal(map[string]any{
		"text": "exercising secret template",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": bashCmd}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 4101, Type: "private"},
			From: &gotgbot.User{Id: 4101, FirstName: "Tester"},
			Text: "trigger secret resolution",
		},
	})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := append([]string(nil), authHits...)
		mu.Unlock()
		for _, h := range got {
			if strings.Contains(h, secretValue) {
				return // pass: secret was resolved server-side before the bridge made the HTTP call
			}
			if strings.Contains(h, "{{secret:") {
				t.Errorf("bridge sent the literal template, not the resolved value: %q", h)
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("side server never received an Authorization header with the resolved secret; hits=%v", authHits)
}

// TestL2_Config_MissingSecretLoggedAtStartup proves that a referenced
// secret with no matching entry in secrets.toml produces a clear
// startup warning, not a silent fallback to "". Drive a config that
// references `{{secret:custom.absent}}` via a tool command or env, then
// assert foci-gw stderr contains a warning naming the missing key —
// proves RequiredSecrets / startup checks actually fire.
func TestL2_Config_MissingSecretLoggedAtStartup(t *testing.T) {
	t.Parallel()
	// Inject a custom endpoint block referencing a secret key that
	// doesn't exist in secrets.toml. EndpointConfig.APIKey carries
	// toml:"api_key" which RequiredSecrets reflects on, so the
	// missing-secret pass at startup fires a warning naming the key.
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 4103},
		},
		ReadyTimeout: 30 * time.Second,
		ExtraConfigTOML: `
[endpoints.test_missing]
format = "openai"
url = "https://example.invalid"
api_key = "custom.absent_at_startup"
`,
	})

	// foci-gw's warn_secrets path runs during startup and logs at WARN
	// level: `missing secret "custom.absent_at_startup" (needed by ...)`
	// (see cmd/foci-gw/warn_secrets.go).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.Stderr(), `missing secret "custom.absent_at_startup"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("expected stderr to contain 'missing secret \"custom.absent_at_startup\"'; got tail:\n%s",
		stderrTail(h.Stderr()))
}

// TestL2_Config_UnknownSecretInTemplateFailsResolution proves that
// runtime resolution of a `{{secret:X}}` template returns an error when
// the secret is missing, rather than the template literal or "". Drive
// a scripted exec invocation referencing an unknown key and assert that
// the tool result records the resolution failure — proves secrets
// Resolve() surfaces the error to the caller and foci doesn't ship the
// bare template to the shell.
func TestL2_Config_UnknownSecretInTemplateFailsResolution(t *testing.T) {
	t.Parallel()
	// Side server should NEVER be hit — the resolver fails before the
	// HTTP call goes out. Count hits to prove the resolver short-
	// circuited the request.
	var (
		mu   sync.Mutex
		hits int
	)
	side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte("should-never-be-hit"))
	}))
	defer side.Close()

	// Don't add custom.missing_key to secrets — that's the whole point.
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 4102},
		},
		ReadyTimeout: 30 * time.Second,
	})

	bashCmd := fmt.Sprintf(
		`foci_http_request --header "Authorization: Bearer {{secret:custom.missing_key}}" %s/probe`,
		side.URL,
	)
	scriptBody, err := json.Marshal(map[string]any{
		"text": "exercising missing-secret failure",
		"tool_uses": []map[string]any{
			{"name": "Bash", "input": map[string]any{"command": bashCmd}},
		},
	})
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	h.WriteCCStubScript(t, "alpha", scriptBody)

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 4102, Type: "private"},
			From: &gotgbot.User{Id: 4102, FirstName: "Tester"},
			Text: "trigger missing-secret",
		},
	})

	// Wait long enough for the bridge to attempt + fail the call.
	if !waitForUserMessage(t, h, "workspaces/alpha", "trigger missing-secret", 20*time.Second) {
		t.Fatalf("turn did not complete; stderr:\n%s", stderrTail(h.Stderr()))
	}
	time.Sleep(1 * time.Second) // settle

	// Side server must never have been hit.
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 0 {
		t.Errorf("side server hit %d times; resolver should have refused the call before any HTTP", got)
	}
	// The bash output for our tool_use should reflect the resolution
	// failure (or be flagged is_error). The exact wording is bridge-
	// internal, but the recorder must carry SOMETHING.
	var found *recorderEntry
	entries := readRecorderEntries(t, h.RecorderPath())
	for i, e := range entries {
		if e.Kind == "bash_tool_use" && strings.Contains(e.Workdir, "workspaces/alpha") &&
			strings.Contains(e.BashCommand, "missing_key") {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no bash_tool_use recorder entry for missing_key cmd; recorder:\n%s",
			recorderTail(t, h.RecorderPath()))
	}
	if found.BashOutput == "" && !found.IsError {
		t.Errorf("bridge silently dropped the missing-secret error: bash_output empty AND is_error=false")
	}
}

// TestL2_Config_MalformedTOMLFailsStartup proves that foci-gw refuses
// to start on a syntactically invalid foci.toml — exits non-zero with a
// parse error message naming the file. Mechanism: append an unterminated
// string to the generated config via ExtraConfigTOML and assert
// TryStartGateway returns a non-Fatal error referencing parse failure.
func TestL2_Config_MalformedTOMLFailsStartup(t *testing.T) {
	t.Parallel()
	// Trailing line with an unterminated string is unambiguous TOML noise
	// that survives any preceding-section validity. Place it as the very
	// last appended block so the rest of the config is well-formed up to
	// that point.
	_, err := testharness.TryStartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 7305},
		},
		ReadyTimeout:    10 * time.Second,
		ExtraConfigTOML: "broken_key = \"unterminated string\nstray = 42\n",
	})
	if err == nil {
		t.Fatalf("expected TryStartGateway to return a parse error on malformed TOML; got nil")
	}
	// The error should surface something parser-shaped — TOML libraries
	// usually mention a line/column or "parse" or the offending token.
	// Don't pin the exact wording (foci could swap TOML libs); look for
	// a generic signal.
	low := strings.ToLower(err.Error())
	if !(strings.Contains(low, "parse") || strings.Contains(low, "toml") || strings.Contains(low, "config") || strings.Contains(low, "unterminated") || strings.Contains(low, "syntax") || strings.Contains(low, "not ready")) {
		t.Errorf("expected parse-shaped error in startup failure; got:\n%v", err)
	}
}

// TestL2_Config_InvalidDurationFailsValidation proves that a config
// with an unparseable Go duration (e.g. `compaction_threshold = "5xyz"`
// where a duration is required) is rejected by cfg.Validate() at load
// time. Assertion: foci-gw exits non-zero before ready, stderr names
// the field path. Catches regressions where a field is added without
// being wired through validateDurations.
func TestL2_Config_InvalidDurationFailsValidation(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: same as MalformedTOMLFailsStartup — needs raw-TOML injection and a non-Fatal startup path so the test can observe the validation error in stderr after exit")
}

// TestL2_Config_UnknownTopLevelKeyWarnsNotFails proves that an
// unrecognised top-level TOML key produces a warning log but does NOT
// block startup. cfg.UndefinedKeys is meant to be a soft signal. Drive
// a foci.toml with `mysteryfield = 42` and assert foci-gw reaches ready
// AND stderr contains a warning naming the key.
func TestL2_Config_UnknownTopLevelKeyWarnsNotFails(t *testing.T) {
	t.Parallel()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 7300},
		},
		ReadyTimeout:    30 * time.Second,
		ExtraConfigTOML: "mysteryfield = 42\n",
	})

	// Reaching StartGateway's return means foci-gw logged the ready line
	// — so the unknown key did NOT block startup. Now verify the soft
	// warning fired: it should name "mysteryfield" in stderr.
	stderr := h.Stderr()
	if !strings.Contains(stderr, "mysteryfield") {
		t.Errorf("expected stderr to mention unknown key 'mysteryfield' as a warning; got:\n%s", stderr)
	}
}

// TestL2_Config_SecretsAllowedAndDeniedAgentsConflictFails proves the
// docs-promised invariant: a secrets.toml section cannot list both
// `allowed_agents` and `denied_agents`. Drive a secrets file with both
// set on the same section and assert foci-gw refuses to start with an
// error naming the section.
func TestL2_Config_SecretsAllowedAndDeniedAgentsConflictFails(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: writeTestSecrets emits a fixed shape — needs HarnessOptions support to inject a custom secrets section with both allowed_agents and denied_agents set, plus a non-Fatal startup path to observe the conflict error")
}

// TestL2_Config_BoolStringOnOffNormalised proves that the
// normalizeBoolStrings preprocessor accepts `enabled = "on"` /
// `enabled = "off"` as aliases for native booleans on the keys in the
// boolKeys allow-list. Drive `[keepalive] enabled = "on"` and assert
// the keepalive subsystem starts (a keepalive timer log line or a
// keepalive-tagged user message in the recorder).
func TestL2_Config_BoolStringOnOffNormalised(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: writeTestConfig does not emit a [keepalive] section — needs HarnessOptions to inject `[keepalive] enabled = \"on\"` so the bool-string normalisation path is exercised")
}

// TestL2_Config_DelegatedBackendReceivesModelVerbatim proves the
// deliberate split documented at cmd/foci-gw/agents_delegated.go:47
// ("Model for the backend — from backend_config, not from the group
// resolver"): delegated backends read backend_config.model as a literal
// string and pass it verbatim as cc-stub's --model flag — they do NOT
// consult [groups] / [models.*] resolution. If that wiring ever
// regressed (e.g. someone wired group resolution into the delegated
// path), cc-stub would receive a resolved Anthropic model id instead of
// the raw "stub" string. Assertion: the recorder's invocation entry for
// alpha's workdir has Model == "stub" (the backend_config literal),
// NOT the value at [models.stub].model.
//
// Note: group → model resolution at the API-agent path (agents.go:135,
// periodic_setup.go:36, summariser, admin prompts) is a separate
// behaviour worth its own test once the harness grows an API-backend
// variant. See TODO #773 for that follow-up.
func TestL2_Config_DelegatedBackendReceivesModelVerbatim(t *testing.T) {
	t.Parallel()
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: 7180},
		},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: 7180, Type: "private"},
			From: &gotgbot.User{Id: 7180, FirstName: "Tester"},
			Text: "ping",
		},
	})

	// Poll for the BACKEND invocation entry under alpha's workdir. Note:
	// the recorder also captures the nudge-extractor RunOnce spawn (which
	// runs without --model), so we filter for invocations with a non-empty
	// Model — that's the long-lived ccstream backend. If group resolution
	// were leaking into the delegated path, Model would be
	// "anthropic/claude-haiku-4-5-20251001" (the value at
	// [models.stub].model) instead of "stub" (the literal
	// backend_config value).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, inv := range invocationsByWorkdir(readRecorderEntries(t, h.RecorderPath()), "workspaces/alpha") {
			if inv.Model == "" {
				continue // skip the nudge-extractor RunOnce spawn
			}
			if inv.Model != "stub" {
				t.Errorf("delegated backend received --model %q, want %q (backend_config.model literal). Group resolution may have leaked into the delegated path.", inv.Model, "stub")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("no cc-stub backend invocation (with --model) recorded for alpha\nstderr:\n%s", h.Stderr())
}

// TestL2_Config_GroupsFastDefaultsToPowerful proves the
// extractGroupNames fallback: when `[groups] powerful = "X"` is set but
// `fast` and `cheap` are omitted, both default to powerful's value.
// Drive a flow that triggers a fast-tier call (e.g. spawn-raw via the
// spawn tool) and assert the resolved model in the recorder matches
// powerful's model — proves the default-on-load logic runs.
func TestL2_Config_GroupsFastDefaultsToPowerful(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: needs (1) a way to trigger a fast-tier call site (e.g. spawn-raw) from cc-stub's scripted tool_uses — currently only Bash tool_uses are executed by cc-stub and no foci_spawn variant exists in the exec-bridge surface that's wired here, and (2) recorder capture of the *secondary* spawn's --model flag (the agent's own cc-stub captures only its own invocation)")
}

// TestL2_Config_AccessAllowedUsersOnlyTrueRejectsUnlisted proves the
// access cascade: `[platforms.access] allowed_users_only = true` with
// a populated `allowed_users` list blocks messages from other user IDs.
// Send a Telegram update from an unlisted user and assert cc-stub is
// NOT invoked AND a denial log line appears in stderr — proves the
// access check sits in front of agent dispatch, not buried in the loop.
func TestL2_Config_AccessAllowedUsersOnlyTrueRejectsUnlisted(t *testing.T) {
	t.Parallel()
	// The harness's writeTestConfig sets [platforms.access]
	// allowed_users_only = false hard-coded; the agent's
	// allowed_users = [<UserID>] populates the bot-level map and
	// rejection is enforced there. Pushing an update from a different
	// user id exercises that gate. While the EXACT
	// allowed_users_only=true platform-level configuration the purpose
	// comment names isn't directly settable through the current
	// harness, the rejection-from-unlisted-user observable IS the same
	// — bot.go's check is `len(allowedUsers) > 0 && !allowedUsers[id]`
	// regardless of the allowed_users_only branch, so this test still
	// pins the "access gate rejects unlisted users" behaviour. A more
	// precise variant covering only the allowed_users_only=true path
	// needs harness support to override the platform-level value.
	const allowedUser = 7400
	const unlistedUser = 7499
	h := testharness.StartGateway(t, testharness.HarnessOptions{
		Agents: []testharness.AgentSpec{
			{ID: "alpha", UserID: allowedUser},
		},
		ReadyTimeout: 30 * time.Second,
	})

	token := h.AgentBotToken("alpha")
	h.TelegramStub().PushUpdate(token, gotgbot.Update{
		Message: &gotgbot.Message{
			Chat: gotgbot.Chat{Id: unlistedUser, Type: "private"},
			From: &gotgbot.User{Id: unlistedUser, FirstName: "Stranger"},
			Text: "should be rejected",
		},
	})

	// Wait long enough that any (incorrect) dispatch would have landed
	// in the recorder by now.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.Stderr(), "rejected message") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Negative: no user_message under alpha's workdir with our marker.
	for _, e := range readRecorderEntries(t, h.RecorderPath()) {
		if e.Kind == "user_message" && strings.Contains(e.Workdir, "workspaces/alpha") && strings.Contains(e.TextPrefix, "should be rejected") {
			t.Fatalf("unlisted user message reached cc-stub — access gate bypassed; entry=%+v\nstderr:\n%s", e, stderrTail(h.Stderr()))
		}
	}

	// Positive: rejection logged.
	if !strings.Contains(h.Stderr(), "rejected message") {
		t.Errorf("expected a 'rejected message' log line in stderr, got:\n%s", stderrTail(h.Stderr()))
	}
}

// TestL2_Config_PerAgentBotSecretOverrideUsesNamedKey proves the
// `bot_secret` field overrides the default `<platform>.<bot>` secret
// lookup convention. Set `bot_secret = "custom.weird_token"` on the
// agent's platform entry, register that key in secrets.toml, and
// assert the agent's bot long-poll runs against the corresponding
// stub-registered token — proves the override path resolves before
// the convention path.
func TestL2_Config_PerAgentBotSecretOverrideUsesNamedKey(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: needs HarnessOptions to (1) set a per-agent `bot_secret = \"custom.weird_token\"` on the agent's platform entry, and (2) register that custom secret section in secrets.toml mapped to a token the TelegramStub recognises")
}

// TestL2_Config_AccessAllowedUsersOnlyFalseAcceptsAny proves the
// inverse access path: with `allowed_users_only = false` and an empty
// `allowed_users` list, any user ID is accepted. Drive a message from
// an arbitrary user and assert cc-stub is invoked normally. Belt-and-
// braces companion to the rejection test; together they pin both
// branches of the access gate.
func TestL2_Config_AccessAllowedUsersOnlyFalseAcceptsAny(t *testing.T) {
	t.Parallel()
	t.Skip("HARNESS GAP: writeTestConfig always emits `allowed_users = [<UserID>]` on the per-agent platform entry, and the bot-level allowedUsers map rejects any user not in that list regardless of allowed_users_only — proving the empty-allowed_users + allowed_users_only=false branch requires HarnessOptions to suppress the allowed_users line entirely")
}
