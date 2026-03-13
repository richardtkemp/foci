package agent

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
)

// makeJPEG creates a minimal JPEG image with the given dimensions.
func makeJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	return buf.Bytes()
}

// makePNG creates a minimal PNG image with the given dimensions.
func makePNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func TestDownscaleUnderThreshold(t *testing.T) {
	// Proves that images whose pixel count is within the limit are returned byte-for-byte identical with no re-encoding.
	data := makeJPEG(100, 100) // 10,000 pixels
	out, mt := maybeDownscaleImage("", data,"image/jpeg", 100*100)
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s, want image/jpeg", mt)
	}
	if !bytes.Equal(out, data) {
		t.Error("image under threshold should be returned unchanged")
	}
}

func TestDownscaleOverThreshold(t *testing.T) {
	// Proves that images exceeding the pixel limit are re-encoded at reduced dimensions so the output pixel count does not exceed the threshold.
	data := makeJPEG(200, 200) // 40,000 pixels
	maxPixels := 10000         // should trigger downscale

	out, mt := maybeDownscaleImage("", data,"image/jpeg", maxPixels)
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s, want image/jpeg", mt)
	}
	if bytes.Equal(out, data) {
		t.Error("image over threshold should be changed")
	}

	// Verify output dimensions are reduced
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg.Width*cfg.Height > maxPixels+100 { // small rounding tolerance
		t.Errorf("output %dx%d = %d pixels, want <= %d", cfg.Width, cfg.Height, cfg.Width*cfg.Height, maxPixels)
	}
}

func TestDownscalePNGtoJPEG(t *testing.T) {
	// Proves that PNG input exceeding the pixel limit is converted and returned as valid JPEG (not PNG), normalising the media type for API compatibility.
	data := makePNG(300, 300) // 90,000 pixels
	out, mt := maybeDownscaleImage("", data,"image/png", 10000)
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s, want image/jpeg after downscale", mt)
	}
	// Verify it's valid JPEG
	if _, err := jpeg.DecodeConfig(bytes.NewReader(out)); err != nil {
		t.Errorf("output is not valid JPEG: %v", err)
	}
}

func TestDownscaleNonImage(t *testing.T) {
	// Proves that maybeDownscaleImage is a no-op for non-image MIME types, returning the original data and media type unmodified.
	data := []byte("not an image")
	out, mt := maybeDownscaleImage("", data,"application/pdf", 1000)
	if mt != "application/pdf" {
		t.Errorf("mediaType changed to %s", mt)
	}
	if !bytes.Equal(out, data) {
		t.Error("non-image should be unchanged")
	}
}

func TestDownscaleDisabled(t *testing.T) {
	// Proves that passing maxPixels=0 completely disables downscaling, returning even very large images unchanged.
	data := makeJPEG(1000, 1000)
	out, mt := maybeDownscaleImage("", data,"image/jpeg", 0)
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s", mt)
	}
	if !bytes.Equal(out, data) {
		t.Error("disabled downscale should return unchanged")
	}
}

func TestDownscaleCorruptData(t *testing.T) {
	// Proves that corrupt or undecodable image data does not cause a panic or error — it is passed through unchanged as a safe fallback.
	data := []byte("corrupt jpeg data that cannot be decoded")
	out, mt := maybeDownscaleImage("", data,"image/jpeg", 1000)
	if !bytes.Equal(out, data) {
		t.Error("corrupt data should be returned unchanged")
	}
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s", mt)
	}
}
