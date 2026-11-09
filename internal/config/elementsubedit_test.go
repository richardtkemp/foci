package config

import (
	"strings"
	"testing"
)

// ttsFixture is shaped like foci.toml's tts line (#2991): an inline array of
// inline tables on one line, with a trailing comment.
const ttsFixture = `tts = [{endpoint = "https://x/v1", id = "groq-playai", fallback = "neural2", max_chars = 200, speed = 1.0}, { id = "neural2", voice = "en-GB" }] # voices
[voice]
tts_rate = 1.3
`

// TestElementSub_InlineArray proves get/set/unset of a key inside one element
// of an inline array change only that key's text.
func TestElementSub_InlineArray(t *testing.T) {
	v, ok, err := LookupPath(ttsFixture, mustPath(t, "tts[groq-playai].fallback"))
	if err != nil || !ok || v != "neural2" {
		t.Fatalf("get: v=%v ok=%v err=%v", v, ok, err)
	}
	if _, ok, err := LookupPath(ttsFixture, mustPath(t, "tts[neural2].fallback")); err != nil || ok {
		t.Errorf("get absent: ok=%v err=%v", ok, err)
	}

	next := mustPlan(t)(PlanUnsetPath(ttsFixture, mustPath(t, "tts[groq-playai].fallback")))
	want := strings.Replace(ttsFixture, `fallback = "neural2", `, "", 1)
	if next != want {
		t.Errorf("unset got:\n%s\nwant:\n%s", next, want)
	}

	next = mustPlan(t)(PlanSetPath(ttsFixture, mustPath(t, "tts[groq-playai].max_chars"), "300"))
	want = strings.Replace(ttsFixture, "max_chars = 200", "max_chars = 300", 1)
	if next != want {
		t.Errorf("set got:\n%s\nwant:\n%s", next, want)
	}

	next = mustPlan(t)(PlanSetPath(ttsFixture, mustPath(t, "tts[1].max_chars"), "4500"))
	want = strings.Replace(ttsFixture, `voice = "en-GB" }`, `voice = "en-GB", max_chars = 4500 }`, 1)
	if next != want {
		t.Errorf("set new key got:\n%s\nwant:\n%s", next, want)
	}

	// A nested key creates the sub-table inside the element.
	next = mustPlan(t)(PlanSetPath(ttsFixture, mustPath(t, "tts[neural2].words.foci"), `"foe-sigh"`))
	got, ok, err := LookupPath(next, mustPath(t, "tts[neural2].words.foci"))
	if err != nil || !ok || got != "foe-sigh" {
		t.Errorf("nested set: got=%v ok=%v err=%v\n%s", got, ok, err, next)
	}
	if !strings.Contains(next, `id = "groq-playai", fallback = "neural2"`) {
		t.Errorf("nested set touched the other element:\n%s", next)
	}
}

// TestElementSub_ArrayOfTables edits a key inside one [[...]] block.
func TestElementSub_ArrayOfTables(t *testing.T) {
	v, ok, err := LookupPath(aotFixture, mustPath(t, "blocked_paths[root].path"))
	if err != nil || !ok || v != "/root" {
		t.Fatalf("get: v=%v ok=%v err=%v", v, ok, err)
	}

	next := mustPlan(t)(PlanSetPath(aotFixture, mustPath(t, "blocked_paths[root].path"), `"/root2"`))
	want := strings.Replace(aotFixture, "path = \"/root\"\n", "path = \"/root2\"\n", 1)
	if next != want {
		t.Errorf("set got:\n%s\nwant:\n%s", next, want)
	}

	next = mustPlan(t)(PlanSetPath(aotFixture, mustPath(t, "blocked_paths[etc].mode"), `"ro"`))
	want = strings.Replace(aotFixture, "path = \"/etc\" # system\n", "path = \"/etc\" # system\nmode = \"ro\"\n", 1)
	if next != want {
		t.Errorf("set new key got:\n%s\nwant:\n%s", next, want)
	}

	next = mustPlan(t)(PlanUnsetPath(aotFixture, mustPath(t, "blocked_paths[etc].path")))
	want = strings.Replace(aotFixture, "path = \"/etc\" # system\n", "", 1)
	if next != want {
		t.Errorf("unset got:\n%s\nwant:\n%s", next, want)
	}
}

// TestElementSub_Refusals covers the errors a user can hit.
func TestElementSub_Refusals(t *testing.T) {
	for _, tc := range []struct{ content, path, want string }{
		{ttsFixture, "tts[neural2].fallback", "not set"},
		{ttsFixture, "tts[nope].fallback", "no element"},
		{"[a]\nlist = [\"x\", \"y\"]\n", "a.list[x].k", "not a table"},
	} {
		_, err := PlanUnsetPath(tc.content, mustPath(t, tc.path))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("unset %s: err = %v, want %q", tc.path, err, tc.want)
		}
	}
	if _, err := PlanSetPath(ttsFixture, mustPath(t, "tts[neural2].voice.x"), "1"); err == nil || !strings.Contains(err.Error(), "not a table") {
		t.Errorf("set under a string: err = %v", err)
	}
	if _, err := PlanAddElement(ttsFixture, mustPath(t, "tts[neural2].voice"), `"x"`); err == nil {
		t.Error("add with a selector and a key was accepted")
	}
	if _, err := ParseKeyPath("a.list[0].b[1]"); err == nil {
		t.Error("a second selector parsed")
	}
	if _, err := ParseKeyPath("a.list[0]x"); err == nil {
		t.Error("text after a selector without '.' parsed")
	}
}

func TestParseKeyPath_KeyAfterSelector(t *testing.T) {
	for in, want := range map[string]string{
		"tts[groq-playai].fallback":        "tts[groq-playai].fallback",
		"a.list[2] . b.c":                  "a.list[2].b.c",
		`agents[main].rules[name=x]."a b"`: `agents[main].rules[name=x]."a b"`,
		`agents[main].rules["a]b"].reason`: `agents[main].rules["a]b"].reason`,
	} {
		p, err := ParseKeyPath(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if p.String() != want {
			t.Errorf("%s -> %s, want %s", in, p.String(), want)
		}
		if back, err := ParseKeyPath(p.String()); err != nil || back.String() != want {
			t.Errorf("%s does not round-trip: %v", want, err)
		}
	}
}
