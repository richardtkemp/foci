package gemini

import (
	"path/filepath"
	"reflect"
	"testing"

	"foci/internal/delegator/accounting"

	"google.golang.org/genai"
)

// #2273: thoughts are billed as output and the tool-use prompt as input; a
// count genai adds that foci does not map raises invUnseenField.

// TestResponseFromGenai_ThoughtsAndToolUsePromptAreBilled: genai documents
// totalTokenCount = prompt + candidates + toolUsePrompt + thoughts.
func TestResponseFromGenai_ThoughtsAndToolUsePromptAreBilled(t *testing.T) {
	resp := &genai.GenerateContentResponse{
		ResponseID: "resp_think",
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 1000, CachedContentTokenCount: 400, CandidatesTokenCount: 20,
			ThoughtsTokenCount: 300, ToolUsePromptTokenCount: 7, TotalTokenCount: 1327,
		},
	}
	result, err := responseFromGenai(resp, "gemini-2.5-pro")
	if err != nil {
		t.Fatal(err)
	}
	u := result.Usage
	if u.InputTokens != 607 || u.OutputTokens != 320 || u.CacheReadInputTokens != 400 {
		t.Errorf("usage = %+v, want input 607 (1000-400+7), output 320 (20+300), cache read 400", u)
	}
	if sum := u.InputTokens + u.OutputTokens + u.CacheReadInputTokens; sum != 1327 {
		t.Errorf("classes sum to %d, want totalTokenCount 1327", sum)
	}
}

// TestUnmappedCounts: a non-zero integer count field foci does not map is
// reported under its JSON name; zero counts and non-count fields are not.
func TestUnmappedCounts(t *testing.T) {
	type future struct {
		PromptTokenCount int32  `json:"promptTokenCount,omitempty"`
		AudioTokenCount  int32  `json:"audioTokenCount,omitempty"`
		VideoTokenCount  int32  `json:"videoTokenCount,omitempty"`
		TrafficType      string `json:"trafficType,omitempty"`
	}
	got := unmappedCounts(&future{PromptTokenCount: 5, AudioTokenCount: 9, TrafficType: "x"}, geminiMappedCounts)
	want := []accounting.UnseenKey{{Path: "audioTokenCount", Value: "9"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestResponseFromGenai_EveryCountIsMapped: genai v1.48's usage metadata with
// every count set raises no invUnseenField — each one is booked.
func TestResponseFromGenai_EveryCountIsMapped(t *testing.T) {
	var alarms []accounting.Alarm
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{OnAlarm: func(a accounting.Alarm) {
		alarms = append(alarms, a)
	}})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })

	md := &genai.GenerateContentResponseUsageMetadata{}
	v := reflect.ValueOf(md).Elem()
	for i := range v.NumField() {
		if v.Field(i).CanInt() {
			v.Field(i).SetInt(int64(i + 1))
		}
	}
	if _, err := responseFromGenai(&genai.GenerateContentResponse{ResponseID: "r", UsageMetadata: md}, "gemini-2.5-pro"); err != nil {
		t.Fatal(err)
	}
	if len(alarms) != 0 {
		t.Errorf("alarms on a fully-populated usage: %+v", alarms)
	}
}
