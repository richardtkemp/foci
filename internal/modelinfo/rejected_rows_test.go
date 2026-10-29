package modelinfo

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// #2583: a row modelinfo rejects is skipped with a report, never fatal, and
// every other row loads exactly as if the bad lines were absent from the file.

const cleanRowsGoodAB = `{"id":"good-a","provider":"openrouter","input_per_1m":1,"output_per_1m":2,"fetched":"2026-09-01"}
{"id":"good-b","provider":"openrouter","input_per_1m":3,"output_per_1m":4,"price_tiers":[{"min_prompt_tokens":200000,"input_per_1m":6,"output_per_1m":8}],"fetched":"2026-09-01"}`

const badRowsMixed = `{"id":"broken-json","provider":"openrouter",
{"provider":"openrouter","input_per_1m":1}
{"id":"bad-x","provider":"openrouter","input_per_1m":5,"price_tiers":[{"min_prompt_tokens":0,"input_per_1m":9}],"fetched":"2026-10-01"}`

func assertSameLoad(t *testing.T, withBad, clean string) []RejectedRow {
	t.Helper()
	reg, hist, known, rejected := parseModelsJSONL([]byte(withBad))
	wantReg, wantHist, wantKnown, wantRejected := parseModelsJSONL([]byte(clean))
	if wantRejected != nil {
		t.Fatalf("control file has rejected rows: %v", wantRejected)
	}
	if !reflect.DeepEqual(reg, wantReg) {
		t.Errorf("registry differs from the file without the bad rows:\n got %v\nwant %v", reg, wantReg)
	}
	if !reflect.DeepEqual(hist, wantHist) {
		t.Errorf("history differs from the file without the bad rows:\n got %v\nwant %v", hist, wantHist)
	}
	if !reflect.DeepEqual(known, wantKnown) {
		t.Errorf("known fields differ from the file without the bad rows:\n got %v\nwant %v", known, wantKnown)
	}
	return rejected
}

func TestParseSkipsRejectedRows(t *testing.T) {
	file := strings.Join([]string{"{\"id\":\"good-a\",\"provider\":\"openrouter\",\"input_per_1m\":1,\"output_per_1m\":2,\"fetched\":\"2026-09-01\"}",
		badRowsMixed,
		strings.Split(cleanRowsGoodAB, "\n")[1]}, "\n")
	rejected := assertSameLoad(t, file, cleanRowsGoodAB)

	reg, _, _, _ := parseModelsJSONL([]byte(file))
	if _, ok := reg["bad-x"]; ok {
		t.Errorf("bad-x (only row rejected) has a registry entry: %v", reg["bad-x"])
	}
	if len(rejected) != 3 {
		t.Fatalf("rejected = %v, want 3 rows", rejected)
	}
	for i, want := range []struct {
		line          int
		id, reason, s string
	}{
		{2, "", "not valid JSON", "line 2 ({\"id\":\"broken-json\""},
		{3, "", "entry has no id", "line 3 ({\"provider\":\"openrouter\""},
		{4, "bad-x", "min_prompt_tokens must be > 0, got 0", `id "bad-x"`},
	} {
		r := rejected[i]
		if r.Line != want.line || r.ID != want.id || !strings.Contains(r.Reason, want.reason) {
			t.Errorf("rejected[%d] = %+v, want line %d id %q reason ~%q", i, r, want.line, want.id, want.reason)
		}
		if s := r.String(); !strings.Contains(s, want.s) || !strings.Contains(s, want.reason) {
			t.Errorf("rejected[%d].String() = %q, want it to name %q and %q", i, s, want.s, want.reason)
		}
	}
}

// Dick 2026-10-28: skip ONLY the bad row; the model keeps its good rows, in
// either order.
func TestParseMixedModelKeepsGoodRow(t *testing.T) {
	good := `{"id":"mix","provider":"openrouter","input_per_1m":1,"output_per_1m":2,"fetched":"2026-09-01"}`
	bad := `{"id":"mix","provider":"openrouter","input_per_1m":7,"output_per_1m":7,"price_tiers":[{"min_prompt_tokens":0,"input_per_1m":9}],"fetched":"2026-10-01"}`
	badOlder := strings.Replace(bad, "2026-10-01", "2026-08-01", 1)
	for _, tc := range []struct{ name, file string }{
		{"bad row newer", good + "\n" + bad},
		{"bad row older", badOlder + "\n" + good},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejected := assertSameLoad(t, tc.file, good)
			if len(rejected) != 1 || rejected[0].ID != "mix" || !strings.Contains(rejected[0].Reason, "min_prompt_tokens") {
				t.Fatalf("rejected = %v, want the one mix tier row", rejected)
			}
			reg, hist, known, _ := parseModelsJSONL([]byte(tc.file))
			if rows := hist["mix"][provKey("openrouter", "")]; len(rows) != 1 || rows[0].fetched != "2026-09-01" {
				t.Fatalf("mix history = %+v, want only the 2026-09-01 row", rows)
			}
			swapRegistry(t, reg, hist, known)
			for _, at := range []time.Time{time.Now(), time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)} {
				got, ok := CostAsOf("mix", at, Tokens{ClassInput: 1_000_000, ClassOutput: 1_000_000})
				if !ok || got != 3 {
					t.Errorf("CostAsOf(mix, %s) = %v (priced %v), want 3 (input 1 + output 2)", at.Format(time.DateOnly), got, ok)
				}
			}
		})
	}
}

// ValidateModelsJSONL is the loader's own verdict, so the sync cannot drift.
func TestValidateModelsJSONLMatchesLoader(t *testing.T) {
	file := cleanRowsGoodAB + "\n" + badRowsMixed
	_, _, _, fromLoader := parseModelsJSONL([]byte(file))
	if got := ValidateModelsJSONL([]byte(file)); !reflect.DeepEqual(got, fromLoader) || len(got) != 3 {
		t.Fatalf("ValidateModelsJSONL = %v, loader rejected %v", got, fromLoader)
	}
	if got := ValidateModelsJSONL([]byte(cleanRowsGoodAB)); got != nil {
		t.Fatalf("clean file: ValidateModelsJSONL = %v, want nil", got)
	}
}

// Requirement 4 (#2583): the repository's own embedded file must load clean.
// Startup now skips bad rows instead of panicking, so without this test a bad
// row in the checkout would pass `make test` silently.
func TestEmbeddedModelsJSONLHasNoRejectedRows(t *testing.T) {
	for _, r := range ValidateModelsJSONL(builtInData) {
		t.Errorf("internal/modelinfo/models.jsonl: %s", r)
	}
	if got := BuiltInRejected(); len(got) != 0 {
		t.Errorf("BuiltInRejected() = %v, want none", got)
	}
}
