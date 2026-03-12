package agent

import (
	"bytes"
	"image"
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"math"

	"foci/internal/log"

	"golang.org/x/image/draw"
)

// maybeDownscaleImage checks whether the image exceeds maxPixels (width*height)
// and, if so, resizes proportionally and re-encodes as JPEG. Non-image data or
// images under the threshold are returned unchanged. A maxPixels of 0 disables
// downscaling.
func maybeDownscaleImage(sessionKey string, data []byte, mediaType string, maxPixels int) ([]byte, string) {
	if maxPixels <= 0 {
		return data, mediaType
	}

	// Only process known image types
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		// OK
	default:
		return data, mediaType
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, mediaType
	}

	pixels := cfg.Width * cfg.Height
	if pixels <= maxPixels {
		return data, mediaType
	}

	// Decode the full image
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mediaType
	}

	// Compute new dimensions preserving aspect ratio
	scale := math.Sqrt(float64(maxPixels) / float64(pixels))
	newW := int(math.Round(float64(cfg.Width) * scale))
	newH := int(math.Round(float64(cfg.Height) * scale))
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return data, mediaType
	}

	log.Infof("image", "session=%s downscaled %dx%d (%d bytes) to %dx%d (%d bytes)",
		sessionKey, cfg.Width, cfg.Height, len(data), newW, newH, buf.Len())

	return buf.Bytes(), "image/jpeg"
}
