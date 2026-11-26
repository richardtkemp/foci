package browser

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-rod/rod"
)

// Open-source Chromium builds, go-rod's pinned Chromium among them, ship
// without the proprietary H.264 and AAC decoders. A site's video player then
// shows its own error ("Something went wrong"), and an agent reading the page
// concludes there is no video (foci_todo #3263). Chrome and Chrome for
// Testing carry both decoders. foci therefore states the gap where an agent
// reads it: in the start result, and in a snapshot of a page with a <video>
// or <audio> element.

// codecProbeJS reports which proprietary codecs the browser cannot play.
// canPlayType returns "" for a codec it cannot decode.
const codecProbeJS = `() => {
	const v = document.createElement('video');
	const missing = [];
	if (v.canPlayType('video/mp4; codecs="avc1.42E01E"') === '') missing.push('H.264');
	if (v.canPlayType('audio/mp4; codecs="mp4a.40.2"') === '') missing.push('AAC');
	return missing;
}`

// mediaCountJS counts the page's media elements.
const mediaCountJS = `() => document.querySelectorAll('video, audio').length`

// missingCodecs returns the proprietary codecs page's browser cannot play,
// empty when it plays them all.
func missingCodecs(page *rod.Page) ([]string, error) {
	res, err := page.Eval(codecProbeJS)
	if err != nil {
		return nil, fmt.Errorf("probe codecs: %w", err)
	}
	var missing []string
	if err := json.Unmarshal([]byte(res.Value.JSON("", "")), &missing); err != nil {
		return nil, fmt.Errorf("decode codec probe: %w", err)
	}
	return missing, nil
}

// codecGapText describes the missing codecs and how to get them.
func codecGapText(missing []string) string {
	return fmt.Sprintf("This browser cannot play %s (an open-source Chromium build). "+
		"A video or audio player error does not mean the page has no media. "+
		"To play it, set [browser] executable_path to Chrome or Chrome for Testing.",
		strings.Join(missing, " or "))
}

// mediaCodecNote returns a snapshot header line when any of frames holds a
// media element the browser may not be able to play, else "". A failed probe
// gives "": the note is advice, and the snapshot must not fail for it.
func mediaCodecNote(frames []*rod.Page) string {
	if len(frames) == 0 {
		return ""
	}
	missing, err := missingCodecs(frames[0])
	if err != nil || len(missing) == 0 {
		return ""
	}
	for _, f := range frames {
		if mediaCount(f) > 0 {
			return "- Media: " + codecGapText(missing) + "\n"
		}
	}
	return ""
}

// mediaCount returns frame's media element count, 0 when it cannot be read.
// rod can panic evaluating in an iframe that lost its document (#3164).
func mediaCount(frame *rod.Page) (n int) {
	defer func() {
		if recover() != nil {
			n = 0
		}
	}()
	res, err := frame.Eval(mediaCountJS)
	if err != nil {
		return 0
	}
	return res.Value.Int()
}
