package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/log"
	"foci/internal/modelinfo"
)

// TestCheckFileContent pins the whole-file check's verdicts and its
// state-freedom: it runs Load's exact parse + Validate pipeline (so a
// groups value naming no model fails exactly as the next restart would fail),
// unknown keys do not fail it, and — like Load since #2356 — neither a passing
// nor a failing check sets log.DebugLogKeySuffix or merges [[modelinfo]] into
// the modelinfo registry.
func TestCheckFileContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foci.toml")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	valid := "data_dir = \"/tmp\"\n"
	if err := CheckFileContent(valid, path, nil); err != nil {
		t.Errorf("valid content: %v", err)
	}

	broken := "[groups]\npowerful = \"nosuchmodel\"\n"
	err := CheckFileContent(broken, path, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid config") || !strings.Contains(err.Error(), "powerful") {
		t.Errorf("groups-broken content error = %v, want the Validate error naming powerful", err)
	}

	unknownKeys := "[nosuchsection]\nfoo = \"bar\"\n"
	if err := CheckFileContent(unknownKeys, path, nil); err != nil {
		t.Errorf("unknown keys must not fail the check: %v", err)
	}

	// Side-effect proofs: content that Load would act on globally. It also
	// carries a group and an agent so the check runs decodeConfig's per-group
	// and per-agent loops — the state-freedom claim must cover the whole
	// pipeline, not only the paths a minimal config reaches.
	sideEffecting := `[debug]
log_api_key_suffix = true

[groups]
powerful = "anthropic/claude-haiku-4-5-20251001"

[[agents]]
id = "clutch"

[[modelinfo]]
id = "checkfile-probe"
context_window = 1000
input_per_1m = 1
output_per_1m = 1
can_effort = true
`

	origLogSuffix := log.DebugLogKeySuffix
	log.DebugLogKeySuffix = false
	modelinfo.ResetToBuiltIn()
	defer func() {
		log.DebugLogKeySuffix = origLogSuffix
		modelinfo.ResetToBuiltIn()
	}()

	if err := CheckFileContent(sideEffecting, path, nil); err != nil {
		t.Fatalf("side-effecting content must pass the check: %v", err)
	}
	if log.DebugLogKeySuffix {
		t.Error("the check must not set log.DebugLogKeySuffix")
	}
	if effort, _, _ := modelinfo.Capabilities("checkfile-probe"); effort {
		t.Error("the check must not merge [[modelinfo]] into the registry")
	}

	// Load on the same content does neither either (#2356: Load has no
	// process-global side effects; foci-gw main applies them).
	write(sideEffecting)
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if log.DebugLogKeySuffix {
		t.Error("Load must not set log.DebugLogKeySuffix")
	}
	if effort, _, _ := modelinfo.Capabilities("checkfile-probe"); effort {
		t.Error("Load must not merge [[modelinfo]] into the registry")
	}
	if cfg.SourcePath != path {
		t.Errorf("SourcePath = %q, want %q", cfg.SourcePath, path)
	}
	// Positive control: applying the loaded config the way main does flips
	// both probes, so the "not set" verdicts above observe the globals.
	log.DebugLogKeySuffix = DerefBool(cfg.Debug.LogAPIKeySuffix)
	ApplyModelInfo(cfg.ModelInfo)
	if !log.DebugLogKeySuffix {
		t.Error("premise: applying the config must set log.DebugLogKeySuffix")
	}
	if effort, _, _ := modelinfo.Capabilities("checkfile-probe"); !effort {
		t.Error("premise: ApplyModelInfo must merge [[modelinfo]] into the registry")
	}

	// A FAILING check or Load on content that asks for the flag leaves it
	// untouched: a pipeline that set the global before Validate (as Load
	// once did) would leak it here.
	log.DebugLogKeySuffix = false
	brokenWithDebug := broken + "\n[debug]\nlog_api_key_suffix = true\n"
	if err := CheckFileContent(brokenWithDebug, path, nil); err == nil {
		t.Fatal("CheckFileContent(broken) must fail")
	}
	if log.DebugLogKeySuffix {
		t.Error("a failing check must not set log.DebugLogKeySuffix")
	}
	write(brokenWithDebug)
	if _, err := Load(path, nil); err == nil {
		t.Fatal("Load(broken) must fail")
	}
	if log.DebugLogKeySuffix {
		t.Error("a failing Load must not set log.DebugLogKeySuffix")
	}
}
