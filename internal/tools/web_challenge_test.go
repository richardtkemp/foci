package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runWebFetch serves body with the given status and headers over httptest and
// runs it through the web_fetch tool with the given extra params.
func runWebFetch(t *testing.T, status int, header map[string]string, body []byte, raw bool) (ToolResult, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	params, _ := json.Marshal(map[string]interface{}{"url": server.URL, "raw": raw})
	return NewWebFetchTool().Execute(context.Background(), params)
}

func TestWebFetchBotChallengeIsAnError(t *testing.T) {
	// #1889: a bot-shield interstitial must come back as an error naming the
	// shield, never as a successful result carrying the challenge page's text.
	// Fixtures are real pages captured 2026-09-26 with web_fetch's own
	// User-Agent. They are served as HTTP 200 with no vendor headers, so the
	// BODY signature alone has to catch each one (the worst case: some shields
	// answer 200, and a proxy can strip headers).
	t.Parallel()
	cases := []struct {
		file, vendor string
	}{
		{"bunny_xdaforums.html", "Bunny Shield"},
		{"vercel_wtwco.html", "Vercel"},
		{"cloudflare_indeed.html", "Cloudflare"},
		{"datadome_g2.html", "DataDome"},
		{"reddit_js_challenge.html", "JavaScript challenge"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			t.Parallel()
			body, err := os.ReadFile(filepath.Join("testdata", "webfetch_challenge", c.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			for _, raw := range []bool{false, true} {
				res, err := runWebFetch(t, http.StatusOK, nil, body, raw)
				if err == nil {
					t.Fatalf("raw=%v: expected an error for a %s challenge page, got success:\n%s", raw, c.vendor, truncateRunes(res.Text, 300))
				}
				if !strings.Contains(err.Error(), c.vendor) || !strings.Contains(err.Error(), "NOT retrieved") {
					t.Errorf("raw=%v: error should name %q and say the content was NOT retrieved, got: %v", raw, c.vendor, err)
				}
			}
		})
	}
}

func TestWebFetchBotChallengeHeader(t *testing.T) {
	// #1889: the vendor's own mitigation header is authoritative even when
	// the body carries no recognisable marker.
	t.Parallel()
	cases := []struct {
		header, vendor string
	}{
		{"Cf-Mitigated", "Cloudflare"},
		{"X-Vercel-Mitigated", "Vercel"},
	}
	for _, c := range cases {
		res, err := runWebFetch(t, http.StatusForbidden, map[string]string{c.header: "challenge"},
			[]byte("<html><body><p>one moment</p></body></html>"), false)
		if err == nil {
			t.Fatalf("%s: challenge header: expected an error, got success: %q", c.header, res.Text)
		}
		if !strings.Contains(err.Error(), c.vendor) || !strings.Contains(err.Error(), "HTTP 403") {
			t.Errorf("%s: error should name %q and the status, got: %v", c.header, c.vendor, err)
		}
	}
}

func TestWebFetchChallengePhraseInArticleIsNotFlagged(t *testing.T) {
	// Negative control: an article that merely DISCUSSES bot shields (quoting
	// every signature phrase) must not be mistaken for one. The signatures
	// only count on a page with almost no visible text.
	t.Parallel()
	para := "<p>Sites fronted by a bot shield show interstitials such as 'Vercel Security Checkpoint', " +
		"'Checking your browser', 'Enable JavaScript and cookies to continue', DDoS-Guard pages, " +
		"assets under /.bunny-shield/, scripts from captcha-delivery.com, a _cf_chl_opt variable " +
		"or a hidden input name=\"js_challenge\". A fetcher should report these honestly.</p>"
	body := []byte("<html><body><article><h1>About bot shields</h1>" + strings.Repeat(para, 6) + "</article></body></html>")
	res, err := runWebFetch(t, http.StatusOK, nil, body, false)
	if err != nil {
		t.Fatalf("an article about bot shields was flagged as one: %v", err)
	}
	if !strings.Contains(res.Text, "About bot shields") {
		t.Errorf("article text missing: %q", truncateRunes(res.Text, 300))
	}
}

func TestWebFetchEmptyResultIsAnError(t *testing.T) {
	// #1889 (2026-09-26 Reddit case): a fetch whose result is empty must be an
	// error, not a silent zero-length success.
	t.Parallel()
	cases := []struct {
		name string
		body string
		raw  bool
	}{
		{"script-only page", `<html><head><title>x</title><script>document.write("hi")</script></head><body><div id="root"></div></body></html>`, false},
		{"empty body", "", false},
		{"empty body raw", "", true},
	}
	for _, c := range cases {
		res, err := runWebFetch(t, http.StatusOK, nil, []byte(c.body), c.raw)
		if err == nil {
			t.Errorf("%s: expected an error for an empty result, got success: %q", c.name, res.Text)
			continue
		}
		if !strings.Contains(err.Error(), "NOT retrieved") || !strings.Contains(err.Error(), "HTTP 200") {
			t.Errorf("%s: error should say the content was NOT retrieved and give the status, got: %v", c.name, err)
		}
	}
}

func TestWebFetchErrorStatusIsFlagged(t *testing.T) {
	// #1889: a non-2xx page (e.g. Reddit's 403 "Blocked" page) still returns
	// its body (TestWebFetchServerError), but the result must lead with the
	// status so the caller can't read the error page as the article.
	t.Parallel()
	res, err := runWebFetch(t, http.StatusForbidden, nil,
		[]byte("<html><head><title>Blocked</title></head><body><p>whoa there, pardner! Your request has been blocked due to a network policy.</p></body></html>"), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(res.Text, "_[foci: HTTP 403") {
		t.Errorf("result should lead with the HTTP status note, got: %q", truncateRunes(res.Text, 200))
	}
	if !strings.Contains(res.Text, "pardner") {
		t.Errorf("error page body should still be returned: %q", res.Text)
	}
}
