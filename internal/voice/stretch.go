package voice

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Local speech-rate change for providers that ignore the API's speed field.
//
// #2937: Groq's Orpheus endpoint (canopylabs/orpheus-v1-english) validates
// "speed" (it rejects a value over 5.0) but returns byte-identical audio for
// 0.5, 1.0, 1.3 and 2.0 (live test, 2026-11-07). Its only response format is
// WAV, so foci changes the rate itself on the decoded PCM. The method is WSOLA
// (waveform-similarity overlap-add): it changes the duration and keeps the
// pitch, where a plain resample would make the voice higher or lower.

const (
	wsolaWindow = 0.030 // analysis window, seconds
	wsolaSearch = 0.010 // how far a segment may move to match the waveform, seconds
)

// parsePCM16WAV returns the interleaved 16-bit little-endian PCM data of a WAV
// file, its sample rate and channel count. It walks the chunks rather than
// assuming a 44-byte header, and accepts a data size larger than the file
// (Groq sends 0xFFFFFFFF, a streamed header) by reading to the end.
func parsePCM16WAV(b []byte) (pcm []byte, sampleRate, channels int, err error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, 0, errors.New("not a RIFF/WAVE file")
	}
	var haveFmt bool
	for off := 12; off+8 <= len(b); {
		id := string(b[off : off+4])
		size := int(binary.LittleEndian.Uint32(b[off+4 : off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > len(b) {
				return nil, 0, 0, errors.New("short fmt chunk")
			}
			format := binary.LittleEndian.Uint16(b[body:])
			channels = int(binary.LittleEndian.Uint16(b[body+2:]))
			sampleRate = int(binary.LittleEndian.Uint32(b[body+4:]))
			bits := binary.LittleEndian.Uint16(b[body+14:])
			if format != 1 || bits != 16 || channels < 1 || sampleRate < 1000 {
				return nil, 0, 0, fmt.Errorf("unsupported WAV format=%d bits=%d channels=%d rate=%d", format, bits, channels, sampleRate)
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return nil, 0, 0, errors.New("data chunk before fmt chunk")
			}
			end := body + size
			if size < 0 || end > len(b) || end < body {
				end = len(b)
			}
			frame := 2 * channels
			end -= (end - body) % frame
			return b[body:end], sampleRate, channels, nil
		}
		if size < 0 || body+size > len(b) || body+size < body {
			break
		}
		off = body + size + size%2 // chunks are padded to an even length
	}
	return nil, 0, 0, errors.New("no data chunk")
}

// stretchWAV returns wav played rate times faster (rate 1.3 is 30% shorter),
// at the same pitch, as a plain 44-byte-header PCM16 WAV. A body that is not
// 16-bit PCM WAV is an error, so the caller can keep the original audio.
func stretchWAV(wav []byte, rate float64) ([]byte, error) {
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return nil, fmt.Errorf("invalid rate %v", rate)
	}
	pcm, sr, ch, err := parsePCM16WAV(wav)
	if err != nil {
		return nil, err
	}
	n := len(pcm) / 2
	in := make([]float64, n)
	for i := 0; i < n; i++ {
		in[i] = float64(int16(binary.LittleEndian.Uint16(pcm[2*i:]))) // #nosec G115 - reinterpret 16-bit sample bits
	}
	out := wsola(in, ch, sr, rate)
	res := make([]byte, 2*len(out))
	for i, v := range out {
		v = math.Round(v)
		if v > math.MaxInt16 {
			v = math.MaxInt16
		} else if v < math.MinInt16 {
			v = math.MinInt16
		}
		binary.LittleEndian.PutUint16(res[2*i:], uint16(int16(v))) // #nosec G115 - clamped to int16 above
	}
	return wrapPCMInWAV(res, sr, ch, 16), nil
}

// wsola time-scales interleaved samples (ch channels) by 1/rate. Each output
// frame of `win` samples is a Hann-windowed input segment placed at a hop of
// win/2, so neighbours sum to unity. The segment is taken near its nominal
// input position (k × hop × rate), moved by up to `search` frames to where its
// first half best matches the natural continuation of the previous segment.
// That match is what avoids phase clicks. The first frame's leading half and
// the last frame's trailing half are not faded, and the input left after the
// last frame is copied unchanged, so nothing at either end is cut.
func wsola(in []float64, ch, sr int, rate float64) []float64 {
	frames := len(in) / ch
	win := int(wsolaWindow * float64(sr))
	win -= win % 2
	hop := win / 2
	search := int(wsolaSearch * float64(sr))
	if rate == 1 || frames < 2*win+search {
		return append([]float64(nil), in[:frames*ch]...)
	}

	mono := make([]float64, frames)
	for f := 0; f < frames; f++ {
		for c := 0; c < ch; c++ {
			mono[f] += in[f*ch+c]
		}
	}
	hann := make([]float64, win)
	for i := range hann {
		hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(win))
	}

	ha := float64(hop) * rate
	out := make([]float64, (int(float64(frames)/rate)+2*win)*ch)
	prev := 0
	k := 0
	for {
		// Frame k at output position k*hop, from input segment start `pos`.
		pos := 0
		if k > 0 {
			nominal := int(math.Round(float64(k) * ha))
			if nominal+win+search > frames {
				break
			}
			pos = bestMatch(mono, prev+hop, nominal, search, hop)
		}
		o := k * hop
		for i := 0; i < win; i++ {
			w := hann[i]
			if k == 0 && i < hop {
				w = 1
			}
			for c := 0; c < ch; c++ {
				out[(o+i)*ch+c] += w * in[(pos+i)*ch+c]
			}
		}
		prev = pos
		k++
	}

	// The last frame's trailing half is the only signal there: undo its fade,
	// then append the input that follows that segment.
	lastO := (k - 1) * hop
	for i := hop; i < win; i++ {
		for c := 0; c < ch; c++ {
			out[(lastO+i)*ch+c] = in[(prev+i)*ch+c]
		}
	}
	end := (lastO + win) * ch
	out = append(out[:end], in[(prev+win)*ch:frames*ch]...)
	return out
}

// bestMatch returns the start in [nominal-search, nominal+search] whose first
// `length` mono samples correlate best (normalised by energy) with the
// samples at `natural`, the continuation of the previous segment.
func bestMatch(mono []float64, natural, nominal, search, length int) int {
	if natural+length > len(mono) {
		return nominal
	}
	ref := mono[natural : natural+length]
	best, bestScore := nominal, math.Inf(-1)
	lo := nominal - search
	if lo < 0 {
		lo = 0
	}
	for c := lo; c <= nominal+search && c+length <= len(mono); c++ {
		var dot, energy float64
		seg := mono[c : c+length]
		for i := 0; i < length; i += 2 { // every other sample: plenty for speech, half the cost
			dot += ref[i] * seg[i]
			energy += seg[i] * seg[i]
		}
		score := dot / math.Sqrt(energy+1e-9)
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}
