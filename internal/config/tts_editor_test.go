package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The app config editor edits [[tts]] (#2946): every key, including each
// voice's replacements map, round-trips through the editor's object-list path
// (TableArrayEntries -> JSON -> ParseObjectListValue -> PlanSetTableArray),
// in both the [[tts]] block layout and the inline `tts = [...]` layout.

func ttsSpec(t *testing.T) ObjectFieldSpec {
	t.Helper()
	spec, ok := ObjectFieldSpecFor("tts")
	if !ok {
		t.Fatal("tts is not a registered object-list section")
	}
	return spec
}

func decodeTTS(t *testing.T, content string) []TTSConfig {
	t.Helper()
	var cfg Config
	if _, err := toml.Decode(content, &cfg); err != nil {
		t.Fatalf("result is not valid TOML: %v\n%s", err, content)
	}
	return cfg.TTS
}

// editorRoundTrip runs content through the editor path: read the entries as the
// schema does, let edit change the decoded JSON entries, then parse and plan.
func editorRoundTrip(t *testing.T, content string, edit func([]map[string]any) []map[string]any) string {
	t.Helper()
	spec := ttsSpec(t)
	path := filepath.Join(t.TempDir(), "foci.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := TableArrayEntries(path, spec)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	var client []map[string]any // what the app decodes and sends back
	if err := json.Unmarshal(wire, &client); err != nil {
		t.Fatal(err)
	}
	client = edit(client)
	back, err := json.Marshal(client)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseObjectListValue(spec, string(back))
	if err != nil {
		t.Fatalf("ParseObjectListValue: %v", err)
	}
	next, err := PlanSetTableArray(content, spec.Section, parsed)
	if err != nil {
		t.Fatalf("PlanSetTableArray: %v", err)
	}
	return next
}

// Every TTSConfig key is editable except the retired fallback (#2974), so a
// key added to the struct later fails here until the editor spec lists it.
func TestTTSObjectSpec_CoversEveryKey(t *testing.T) {
	spec := ttsSpec(t)
	if !spec.Live {
		t.Error("tts spec is not Live, but [[tts]] applies live (#2942)")
	}
	got := map[string]FieldType{}
	for _, f := range spec.Fields {
		got[f.Key] = f.Type
	}
	typ := reflect.TypeOf(TTSConfig{})
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0]
		if key == "fallback" {
			if _, ok := got[key]; ok {
				t.Error("retired key fallback is offered by the editor")
			}
			continue
		}
		if _, ok := got[key]; !ok {
			t.Errorf("TTSConfig key %q is not in the editor's tts spec", key)
		}
	}
	if got["replacements"] != FieldStringMap {
		t.Errorf("replacements type = %v, want FieldStringMap", got["replacements"])
	}
	if FieldStringMap.TypeName() != "map" {
		t.Errorf("FieldStringMap wire name = %q, want map", FieldStringMap.TypeName())
	}
}

func TestTTSEditor_BlockLayoutKeepsReplacementsAndComments(t *testing.T) {
	content := `[voice]
stt = "groq"

# groq voice: fast
[[tts]]
id = "groq"
format = "openai"
voice = "troy"
rate = 1.2

[tts.replacements]
foci = "foki"
Dick = "Dik"

# edge backstop
[[tts]]
id = "edge"
format = "edge-tts"
voice = "en-GB-RyanNeural"
replacements = {clutch = "klutch"}

[keepalive]
enabled = true
`
	next := editorRoundTrip(t, content, func(es []map[string]any) []map[string]any {
		es[1]["voice"] = "en-GB-SoniaNeural"
		es[1]["max_chars"] = float64(300)
		return es
	})
	tts := decodeTTS(t, next)
	if len(tts) != 2 {
		t.Fatalf("got %d voices, want 2\n%s", len(tts), next)
	}
	if want := map[string]string{"foci": "foki", "Dick": "Dik"}; !reflect.DeepEqual(tts[0].Replacements, want) {
		t.Errorf("groq replacements = %v, want %v", tts[0].Replacements, want)
	}
	if tts[0].Rate != 1.2 || tts[0].Voice != "troy" {
		t.Errorf("unedited groq voice changed: %+v", tts[0])
	}
	if want := map[string]string{"clutch": "klutch"}; !reflect.DeepEqual(tts[1].Replacements, want) {
		t.Errorf("edge replacements = %v, want %v", tts[1].Replacements, want)
	}
	if tts[1].Voice != "en-GB-SoniaNeural" || tts[1].MaxChars != 300 {
		t.Errorf("edge edit not written: %+v", tts[1])
	}
	for _, keep := range []string{"# groq voice: fast", "# edge backstop", "[tts.replacements]", "[keepalive]"} {
		if !strings.Contains(next, keep) {
			t.Errorf("lost %q:\n%s", keep, next)
		}
	}
}

func TestTTSEditor_ReplacementsEditAddAndReorder(t *testing.T) {
	content := `[[tts]]
id = "a"
format = "edge-tts"
replacements = {x = "y"}

[[tts]]
id = "b"
format = "edge-tts"
`
	next := editorRoundTrip(t, content, func(es []map[string]any) []map[string]any {
		es[0]["replacements"] = map[string]any{"x": "z", "two words": "w"}
		es = append(es, map[string]any{"id": "c", "format": "openai", "local_speed_control": true, "priority": float64(-1)})
		return []map[string]any{es[1], es[0], es[2]}
	})
	tts := decodeTTS(t, next)
	if len(tts) != 3 || tts[0].ID != "b" || tts[1].ID != "a" || tts[2].ID != "c" {
		t.Fatalf("order = %+v\n%s", tts, next)
	}
	if want := map[string]string{"x": "z", "two words": "w"}; !reflect.DeepEqual(tts[1].Replacements, want) {
		t.Errorf("a replacements = %v, want %v", tts[1].Replacements, want)
	}
	if !tts[2].LocalSpeedControl || tts[2].Priority != -1 {
		t.Errorf("new voice = %+v", tts[2])
	}
}

// Dick's live foci.toml writes [[tts]] as one inline `tts = [...]` line. The
// editor must edit it in place, not append [[tts]] blocks (a duplicate key the
// load check refuses).
func TestTTSEditor_InlineArrayEditedInPlace(t *testing.T) {
	content := `# voices
tts = [{id = "groq", format = "openai", voice = "troy", replacements = {foci = "foki"}}, {id = "neural2", format = "google", max_chars = 4500}]

[keepalive]
enabled = true
`
	next := editorRoundTrip(t, content, func(es []map[string]any) []map[string]any {
		es[1]["voice"] = "en-GB-Neural2-B"
		return es
	})
	tts := decodeTTS(t, next)
	if len(tts) != 2 || tts[1].Voice != "en-GB-Neural2-B" || tts[1].MaxChars != 4500 {
		t.Fatalf("tts = %+v\n%s", tts, next)
	}
	if want := map[string]string{"foci": "foki"}; !reflect.DeepEqual(tts[0].Replacements, want) {
		t.Errorf("groq replacements = %v, want %v", tts[0].Replacements, want)
	}
	if strings.Contains(next, "[[tts]]") || strings.Count(next, "tts = [") != 1 {
		t.Errorf("inline array not edited in place:\n%s", next)
	}
	if !strings.Contains(next, "# voices") || !strings.Contains(next, "[keepalive]") {
		t.Errorf("unrelated content lost:\n%s", next)
	}
}

func TestTTSEditor_InlineArrayClearedRemovesKey(t *testing.T) {
	content := "tts = [{id = \"a\", format = \"edge-tts\"}]\n\n[keepalive]\nenabled = true\n"
	next, err := PlanSetTableArray(content, "tts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tts := decodeTTS(t, next); len(tts) != 0 {
		t.Errorf("tts = %+v, want none\n%s", tts, next)
	}
	if !strings.Contains(next, "[keepalive]\nenabled = true") {
		t.Errorf("unrelated content lost:\n%s", next)
	}
}

func TestParseObjectListValue_StringMapRejectsNonString(t *testing.T) {
	spec := ttsSpec(t)
	if _, err := ParseObjectListValue(spec, `[{"id":"a","replacements":{"x":1}}]`); err == nil {
		t.Error("a non-string replacement value was accepted")
	}
	if _, err := ParseObjectListValue(spec, `[{"id":"a","replacements":"x=y"}]`); err == nil {
		t.Error("a string where a map belongs was accepted")
	}
}
