package all_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"foci/internal/delegator"
	_ "foci/internal/delegator/all"
	"foci/internal/modelcaps"
)

// repoRoot is the module root, relative to this package's directory (the
// working directory of its tests).
const repoRoot = "../../.."

func TestSpecs_RegisteredAndValid(t *testing.T) {
	specs := delegator.Specs()
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
		if err := s.Validate(); err != nil {
			t.Errorf("invalid Spec:\n%v", err)
		}
	}
	if want := "claude-code,codex,opencode"; strings.Join(names, ",") != want {
		t.Errorf("registered backends = %v, want %s", names, want)
	}
}

// proofs names, for every backend, the test that proves each KindBehaviour
// capability it declares Yes (#2154 Q4). A behavioural claim has no method to
// check, so without a named proof nothing stops a backend claiming what it
// never does (cctmux claimed nudges and streaming it never delivered). The
// test must exist in the backend's own package.
var proofs = map[string]map[delegator.Capability]string{
	"claude-code": {
		delegator.CapPostToolNudge:            "TestHandleHookResponse_PostToolNudgeDispatched",
		delegator.CapPreAnswerNudge:           "TestOnResult_PreAnswerReDispatches",
		delegator.CapStreaming:                "TestOnStreamEvent_TextDelta",
		delegator.CapControlModel:             "TestSendControl_SetModel",
		delegator.CapControlEffort:            "TestSendControl_ApplyFlagSettings",
		delegator.CapControlPermissionMode:    "TestSendControl_SetPermissionMode",
		delegator.CapPermissionResponse:       "TestRespondToPermission_Allow",
		delegator.CapUnstartedReadinessProbe:  "TestCheckReady_Authenticated",
		delegator.CapPreToolRules:             "TestBuildHookSettingsJSON_PreToolRules",
		delegator.CapStopRules:                "TestBuildHookSettingsJSON_StopRules",
		delegator.CapRelogin:                  "TestCheckReady_NotAuthenticated_TriggersRelogin",
		delegator.CapCommandApprovalAllowlist: "TestHandlePermissionRequest_AutoApprove",
	},
	"codex": {
		delegator.CapStreaming:                "TestDispatch_StreamingDeltas",
		delegator.CapControlModel:             "TestSendControl_SetModel",
		delegator.CapControlEffort:            "TestSendControl_Effort",
		delegator.CapControlPermissionMode:    "TestSendControl_SetPermissionMode",
		delegator.CapPermissionResponse:       "TestRespondToPermission_ResolvesByItemID",
		delegator.CapUnstartedReadinessProbe:  "TestCheckReady_BinaryInPath",
		delegator.CapCommandApprovalAllowlist: "TestOnCommandApproval_AutoApproveRuleAccepts",
	},
	"opencode": {
		delegator.CapStreaming:                "TestOnMessagePartDelta_TextFiresOnTextDelta",
		delegator.CapControlModel:             "TestSendControl_SetModel",
		delegator.CapControlPermissionMode:    "TestSendControl_SetPermissionMode",
		delegator.CapPermissionResponse:       "TestRespondToPermission_AllowPostsAndResolves",
		delegator.CapCommandApprovalAllowlist: "TestAutoApprove_BashMatchingRule_NoPrompt",
	},
}

func TestBehaviourCapabilities_HaveProofs(t *testing.T) {
	for _, s := range delegator.Specs() {
		dir := packageDir(t, s)
		tests := testFuncs(t, dir)
		named := proofs[s.Name]
		for _, c := range delegator.AllCapabilities() {
			if c.Info().Kind != delegator.KindBehaviour {
				if _, ok := named[c]; ok {
					t.Errorf("%s: proof for %s, which is not a behavioural capability", s.Name, c)
				}
				continue
			}
			proof, ok := named[c]
			switch {
			case s.Supports(c) && !ok:
				t.Errorf("%s declares %s Yes with no proving test: add one to proofs in %s", s.Name, c, "internal/delegator/all/spec_test.go")
			case !s.Supports(c) && ok:
				t.Errorf("%s: stale proof %s for %s, which it does not declare Yes", s.Name, proof, c)
			case ok && !tests[proof]:
				t.Errorf("%s: proof %s for %s is not a test in %s", s.Name, proof, c, dir)
			}
		}
	}
}

// packageDir is the source directory of the backend's package, from its
// Prototype's type.
func packageDir(t *testing.T, s delegator.Spec) string {
	t.Helper()
	pkg := reflect.TypeOf(s.Prototype).Elem().PkgPath()
	rel, ok := strings.CutPrefix(pkg, "foci/")
	if !ok {
		t.Fatalf("%s: prototype package %q is outside the module", s.Name, pkg)
	}
	return filepath.Join(repoRoot, rel)
}

func testFuncs(t *testing.T, dir string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files in %s (err %v)", dir, err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && strings.HasPrefix(fd.Name.Name, "Test") {
				out[fd.Name.Name] = true
			}
		}
	}
	return out
}

// TestSpecs_AgreeWithLegacySources holds the Spec and the mechanisms it will
// replace (#2154 Phase 2) to the same answers until those are deleted: the
// static-data getter interfaces on a live instance, the nudge/streaming
// Capabilities, and modelcaps.BackendKey.
func TestSpecs_AgreeWithLegacySources(t *testing.T) {
	for _, s := range delegator.Specs() {
		be, err := s.New(map[string]any{})
		if err != nil || be == nil {
			t.Fatalf("%s: New = %v, %v", s.Name, be, err)
		}
		if lb, ok := be.(delegator.LedgerBooker); !ok || lb.LedgerBackend() != s.LedgerKey {
			t.Errorf("%s: LedgerBooker disagrees with Spec.LedgerKey %q", s.Name, s.LedgerKey)
		}
		tac, ok := be.(delegator.TurnActivityCloser)
		if got := ok && tac.ClosesTurnActivity(); got != s.ClosesTurnActivity {
			t.Errorf("%s: TurnActivityCloser = %v, Spec.ClosesTurnActivity = %v", s.Name, got, s.ClosesTurnActivity)
		}
		var ttl any
		if p, ok := be.(delegator.CacheTTLProvider); ok {
			ttl = p.CacheTTL()
		}
		if (ttl == nil && s.CacheTTL != 0) || (ttl != nil && ttl != s.CacheTTL) {
			t.Errorf("%s: CacheTTLProvider = %v, Spec.CacheTTL = %v", s.Name, ttl, s.CacheTTL)
		}
		def := ""
		if d, ok := be.(delegator.BatchModelDefaulter); ok {
			def = d.BatchDefaultModel()
		}
		cheap := ""
		if c, ok := be.(delegator.BatchCheapModeler); ok {
			cheap = c.BatchCheapModel()
		}
		if def != s.BatchDefaultModel || cheap != s.BatchCheapModel {
			t.Errorf("%s: batch models (%q, %q), Spec (%q, %q)", s.Name, def, cheap, s.BatchDefaultModel, s.BatchCheapModel)
		}
		rf, ok := be.(delegator.RunningBackendForker)
		if got := ok && rf.ForkRequiresRunningBackend(); got != s.ForkNeedsRunning {
			t.Errorf("%s: RunningBackendForker = %v, Spec.ForkNeedsRunning = %v", s.Name, got, s.ForkNeedsRunning)
		}
		bc, ok := be.(delegator.BackendCapabilities)
		if !ok {
			t.Errorf("%s: no Capabilities() method", s.Name)
		} else if got, want := bc.Capabilities(), delegator.CapabilitiesForBackend(s.Name); got != want {
			t.Errorf("%s: Capabilities() = %+v, CapabilitiesForBackend = %+v", s.Name, got, want)
		}
		if got := modelcaps.BackendKey(s.Name); got != s.ModelcapsKey {
			t.Errorf("%s: modelcaps.BackendKey = %q, Spec.ModelcapsKey = %q", s.Name, got, s.ModelcapsKey)
		}
	}
}

// TestCapabilitiesForBackend pins each backend's declared nudge and streaming
// capabilities to what it implements (#2154).
func TestCapabilitiesForBackend(t *testing.T) {
	tests := []struct {
		backend string
		want    delegator.Capabilities
	}{
		{"claude-code", delegator.Capabilities{PostToolNudge: true, PreAnswerNudge: true, Streaming: true}},
		{"opencode", delegator.Capabilities{Streaming: true}},
		{"codex", delegator.Capabilities{Streaming: true}},
		{"some-future-backend", delegator.Capabilities{}},
	}
	for _, tt := range tests {
		if got := delegator.CapabilitiesForBackend(tt.backend); got != tt.want {
			t.Errorf("CapabilitiesForBackend(%q) = %+v, want %+v", tt.backend, got, tt.want)
		}
	}
}

func TestHumanReadableBackendName(t *testing.T) {
	for backend, want := range map[string]string{
		"claude-code": "Claude Code",
		"codex":       "Codex CLI",
		"opencode":    "OpenCode",
	} {
		if got := delegator.HumanReadableBackendName(backend); got != want {
			t.Errorf("HumanReadableBackendName(%q) = %q, want %q", backend, got, want)
		}
	}
}

const (
	docBegin = "<!-- BEGIN GENERATED CAPABILITIES: internal/delegator/all TestBackendsDoc_CapabilityTable. Edit the Specs, not this block. -->"
	docEnd   = "<!-- END GENERATED CAPABILITIES -->"
)

// TestBackendsDoc_CapabilityTable keeps the capability table in
// docs/BACKENDS.md equal to what the Specs declare, so the docs cannot drift.
// On a mismatch it prints the block to paste between the markers.
func TestBackendsDoc_CapabilityTable(t *testing.T) {
	path := filepath.Join(repoRoot, "docs", "BACKENDS.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i, j := strings.Index(doc, docBegin), strings.Index(doc, docEnd)
	if i < 0 || j < i {
		t.Fatalf("%s: generated-capabilities markers missing; add:\n%s\n%s%s", path, docBegin, renderCapabilityTable(delegator.Specs()), docEnd)
	}
	got := doc[i+len(docBegin) : j]
	want := renderCapabilityTable(delegator.Specs())
	if got != want {
		t.Errorf("%s capability table is stale; replace the block between the markers with:\n%s", path, want)
	}
}

func renderCapabilityTable(specs []delegator.Spec) string {
	var b strings.Builder
	b.WriteString("\n| Capability | Meaning |")
	sep := "\n|---|---|"
	for _, s := range specs {
		b.WriteString(" " + s.Name + " |")
		sep += "---|"
	}
	b.WriteString(sep + "\n")
	why := map[delegator.Capability][]string{}
	for _, c := range delegator.AllCapabilities() {
		info := c.Info()
		b.WriteString("| `" + info.Name + "` | " + escapeCell(info.Doc) + " |")
		for _, s := range specs {
			sup := s.Support(c)
			switch sup.State() {
			case delegator.Supported:
				b.WriteString(" ✓ |")
			case delegator.Unsupported:
				b.WriteString(" ✗ |")
				why[c] = append(why[c], s.Name+": "+sup.Reason)
			case delegator.NotApplicableState:
				b.WriteString(" n/a |")
				why[c] = append(why[c], s.Name+" (n/a): "+sup.Reason)
			default:
				b.WriteString(" ? |")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\nWhy not:\n\n")
	caps := make([]delegator.Capability, 0, len(why))
	for c := range why {
		caps = append(caps, c)
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	for _, c := range caps {
		b.WriteString("- `" + c.String() + "`: " + strings.Join(why[c], "; ") + ".\n")
	}
	b.WriteString("\n")
	return b.String()
}

func escapeCell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }
