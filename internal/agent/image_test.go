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

// TestDownscaleUnderThreshold verifies that images smaller than maxPixels
// are returned unchanged.
func TestDownscaleUnderThreshold(t *testing.T) {
	data := makeJPEG(100, 100) // 10,000 pixels
	out, mt := maybeDownscaleImage(data, "image/jpeg", 100*100)
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s, want image/jpeg", mt)
	}
	if !bytes.Equal(out, data) {
		t.Error("image under threshold should be returned unchanged")
	}
}

// TestDownscaleOverThreshold verifies that images exceeding maxPixels are
// resized and re-encoded as JPEG with smaller dimensions.
func TestDownscaleOverThreshold(t *testing.T) {
	data := makeJPEG(200, 200) // 40,000 pixels
	maxPixels := 10000         // should trigger downscale

	out, mt := maybeDownscaleImage(data, "image/jpeg", maxPixels)
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

// TestDownscalePNGtoJPEG verifies that PNG images are downscaled and
// re-encoded as JPEG.
func TestDownscalePNGtoJPEG(t *testing.T) {
	data := makePNG(300, 300) // 90,000 pixels
	out, mt := maybeDownscaleImage(data, "image/png", 10000)
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s, want image/jpeg after downscale", mt)
	}
	// Verify it's valid JPEG
	if _, err := jpeg.DecodeConfig(bytes.NewReader(out)); err != nil {
		t.Errorf("output is not valid JPEG: %v", err)
	}
}

// TestDownscaleNonImage verifies that non-image media types are returned
// unchanged.
func TestDownscaleNonImage(t *testing.T) {
	data := []byte("not an image")
	out, mt := maybeDownscaleImage(data, "application/pdf", 1000)
	if mt != "application/pdf" {
		t.Errorf("mediaType changed to %s", mt)
	}
	if !bytes.Equal(out, data) {
		t.Error("non-image should be unchanged")
	}
}

// TestDownscaleDisabled verifies that maxPixels=0 disables downscaling.
func TestDownscaleDisabled(t *testing.T) {
	data := makeJPEG(1000, 1000)
	out, mt := maybeDownscaleImage(data, "image/jpeg", 0)
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s", mt)
	}
	if !bytes.Equal(out, data) {
		t.Error("disabled downscale should return unchanged")
	}
}

// TestDownscaleCorruptData verifies that corrupt image data is returned
// unchanged rather than causing an error.
func TestDownscaleCorruptData(t *testing.T) {
	data := []byte("corrupt jpeg data that cannot be decoded")
	out, mt := maybeDownscaleImage(data, "image/jpeg", 1000)
	if !bytes.Equal(out, data) {
		t.Error("corrupt data should be returned unchanged")
	}
	if mt != "image/jpeg" {
		t.Errorf("mediaType = %s", mt)
	}
}
