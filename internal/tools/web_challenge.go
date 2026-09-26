package tools

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

// Bot-shield detection for web_fetch (#1889). A shield (Cloudflare, Bunny,
// Vercel, DataDome, DDoS-Guard, Reddit's JS check) answers a non-browser
// client with an interstitial instead of the page. Extracted to markdown, that
// interstitial is a short, well-formed result — or an empty one — which
// web_fetch used to hand back as success, so an agent could summarise
// "Hold tight, we are checking your browser" as if it were the article. This
// is not an attempt to get past the shield, only to report honestly that the
// content was not fetched.

// challengeHeaders are vendor response headers that mark a challenge
// outright. Authoritative on their own, whatever the body says.
var challengeHeaders = []struct{ header, value, vendor string }{
	{"Cf-Mitigated", "challenge", "Cloudflare"},
	{"X-Vercel-Mitigated", "challenge", "Vercel Security Checkpoint"},
}

// challengeMarkers are body substrings (matched case-insensitively) that
// identify an interstitial. Each also appears on ordinary pages — an article
// about bot shields, a vendor's own homepage — so a marker only counts on a
// page with almost no visible text (challengeMaxVisibleChars).
var challengeMarkers = []struct{ marker, vendor string }{
	{"_cf_chl_opt", "Cloudflare"},
	{"/.bunny-shield/", "Bunny Shield"},
	{"vercel security checkpoint", "Vercel Security Checkpoint"},
	{"captcha-delivery.com", "DataDome"},
	{"ddos-guard", "DDoS-Guard"},
	{`name="js_challenge"`, "a JavaScript challenge"},
	{"enable javascript and cookies to continue", "a browser check"},
	{"checking your browser", "a browser check"},
}

// challengeMaxVisibleChars bounds the visible text of a page whose markers
// are trusted. The captured interstitials run from 0 (Reddit, Cloudflare) to
// ~330 chars (Bunny); a real article is far past this.
const challengeMaxVisibleChars = 1000

// detectBotChallenge names the shield when the response is a bot-challenge
// interstitial rather than the requested content, else returns "".
func detectBotChallenge(h http.Header, body []byte) string {
	for _, c := range challengeHeaders {
		if strings.EqualFold(strings.TrimSpace(h.Get(c.header)), c.value) {
			return c.vendor
		}
	}
	lower := bytes.ToLower(body)
	for _, m := range challengeMarkers {
		if bytes.Contains(lower, []byte(m.marker)) {
			if bodyVisibleTextLen(body) > challengeMaxVisibleChars {
				return ""
			}
			return m.vendor
		}
	}
	return ""
}

// errBotChallenge is the error web_fetch returns for a detected shield.
func errBotChallenge(rawURL string, status int, vendor string) error {
	return fmt.Errorf("web_fetch: %s returned a bot-challenge page (%s, HTTP %d) instead of the requested content — the page was NOT retrieved. Retrying (including raw=true) will get the same page; use another source such as web_search snippets", rawURL, vendor, status)
}

// errEmptyFetch is the error web_fetch returns when there is nothing to
// return: an empty body, or a page with no extractable text (content rendered
// by JavaScript, or a silent block).
func errEmptyFetch(rawURL string, status, bodyLen int) error {
	return fmt.Errorf("web_fetch: %s returned no extractable text (HTTP %d, %d-byte body) — the content was NOT retrieved. The page is probably rendered by JavaScript or the site blocked the request; use another source", rawURL, status, bodyLen)
}

// httpStatusNote leads a markdown result whose status is not 2xx. The body
// is still returned (an error page can say something useful), but the caller
// must not read, say, a 403 "Blocked" page as the article.
func httpStatusNote(status int) string {
	return fmt.Sprintf("_[foci: HTTP %d %s — this is the server's error response, not the requested page.]_\n\n", status, http.StatusText(status))
}
