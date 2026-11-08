package openai

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"foci/internal/delegator/accounting"

	"github.com/openai/openai-go/v3"
)

// #2273: prompt_tokens includes cached_tokens and OpenRouter's
// cache_write_tokens, so input excludes both; a usage that breaks that rule
// alarms; and an unknown usage key raises invUnseenField.

func liveLedger(t *testing.T) *[]accounting.Alarm {
	t.Helper()
	var alarms []accounting.Alarm
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{OnAlarm: func(a accounting.Alarm) {
		alarms = append(alarms, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })
	return &alarms
}

func completion(t *testing.T, usage string) *openai.ChatCompletion {
	t.Helper()
	var c openai.ChatCompletion
	raw := `{"id":"gen-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":` + usage + `}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func alarmsOf(alarms []accounting.Alarm, inv string) []accounting.Alarm {
	var out []accounting.Alarm
	for _, a := range alarms {
		if a.Invariant == inv {
			out = append(out, a)
		}
	}
	return out
}

// TestResponseFromOpenAI_InputExcludesCache: a live deepseek row shape —
// prompt 13966 of which 10903 cached — books 3063 input, so the cached tokens
// are priced once and the context fill is not inflated.
func TestResponseFromOpenAI_InputExcludesCache(t *testing.T) {
	alarms := liveLedger(t)
	resp, err := responseFromOpenAI(completion(t, `{"prompt_tokens":13966,"completion_tokens":50,"total_tokens":14016,"prompt_tokens_details":{"cached_tokens":10903}}`), "deepseek/deepseek-v4-pro")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 3063 || resp.Usage.CacheReadInputTokens != 10903 {
		t.Errorf("usage = %+v, want input 3063 (13966 - 10903), cache read 10903", resp.Usage)
	}
	if len(*alarms) != 0 {
		t.Errorf("alarms: %+v", *alarms)
	}
}

// TestResponseFromOpenAI_CacheBeyondPromptAlarms: cached + cache-written
// beyond prompt_tokens breaks the subset rule: input is 0, never negative,
// and invCachedIsSubsetOfInput is raised on backend api.
func TestResponseFromOpenAI_CacheBeyondPromptAlarms(t *testing.T) {
	alarms := liveLedger(t)
	resp, err := responseFromOpenAI(completion(t, `{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":90,"cache_write_tokens":20}}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 0 {
		t.Errorf("input = %d, want 0", resp.Usage.InputTokens)
	}
	got := alarmsOf(*alarms, accounting.InvCachedIsSubsetOfInput)
	if len(got) != 1 || got[0].Backend != accounting.BackendAPI || !strings.Contains(got[0].Detail, "100") {
		t.Errorf("invCachedIsSubsetOfInput = %+v, want one on backend api", *alarms)
	}
}

// TestResponseFromOpenAI_UnseenUsageKey: CompletionUsage's keys and
// OpenRouter's extensions are known; a key neither has alarms once.
func TestResponseFromOpenAI_UnseenUsageKey(t *testing.T) {
	alarms := liveLedger(t)
	openRouter := `{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"cost":0.0012,"is_byok":false,` +
		`"cost_details":{"upstream_inference_cost":null,"upstream_inference_prompt_cost":0.001},` +
		`"prompt_tokens_details":{"cached_tokens":50,"cache_write_tokens":10,"audio_tokens":0},` +
		`"completion_tokens_details":{"reasoning_tokens":2,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`
	if _, err := responseFromOpenAI(completion(t, openRouter), "m"); err != nil {
		t.Fatal(err)
	}
	if got := alarmsOf(*alarms, accounting.InvUnseenField); len(got) != 0 {
		t.Fatalf("invUnseenField on known keys: %+v", got)
	}
	withNew := strings.Replace(openRouter, `"audio_tokens":0}`, `"audio_tokens":0,"video_tokens":7}`, 1)
	for range 2 {
		if _, err := responseFromOpenAI(completion(t, withNew), "m"); err != nil {
			t.Fatal(err)
		}
	}
	got := alarmsOf(*alarms, accounting.InvUnseenField)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "openai") || !strings.Contains(got[0].Detail, "prompt_tokens_details.video_tokens=7") {
		t.Errorf("invUnseenField = %+v, want one naming openai and prompt_tokens_details.video_tokens=7", got)
	}
}

// TestResponseFromOpenAI_ZeroImageVideoTokens (#2982): the live OpenRouter
// deepseek-v4-pro shape carries completion_tokens_details.image_tokens=0 and
// prompt_tokens_details.video_tokens=0. A zero count leaves nothing unpriced,
// so it raises no invUnseenField; a non-zero image_tokens still does.
func TestResponseFromOpenAI_ZeroImageVideoTokens(t *testing.T) {
	alarms := liveLedger(t)
	live := `{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"cost":0.0012,"is_byok":false,` +
		`"prompt_tokens_details":{"cached_tokens":50,"cache_write_tokens":0,"audio_tokens":0,"video_tokens":0},` +
		`"completion_tokens_details":{"reasoning_tokens":2,"image_tokens":0,"audio_tokens":0}}`
	if _, err := responseFromOpenAI(completion(t, live), "openrouter/deepseek/deepseek-v4-pro:floor"); err != nil {
		t.Fatal(err)
	}
	if got := alarmsOf(*alarms, accounting.InvUnseenField); len(got) != 0 {
		t.Fatalf("invUnseenField on zero-valued keys: %+v", got)
	}
	withImages := strings.Replace(live, `"image_tokens":0`, `"image_tokens":9`, 1)
	if _, err := responseFromOpenAI(completion(t, withImages), "m"); err != nil {
		t.Fatal(err)
	}
	got := alarmsOf(*alarms, accounting.InvUnseenField)
	if len(got) != 1 || !strings.Contains(got[0].Detail, "completion_tokens_details.image_tokens=9") || strings.Contains(got[0].Detail, "video_tokens") {
		t.Errorf("invUnseenField = %+v, want one naming only completion_tokens_details.image_tokens=9", got)
	}
}
