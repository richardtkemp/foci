package voice

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWrapPCMInWAV(t *testing.T) {
	// Create sample PCM data (100 samples of 16-bit mono audio)
	pcm := make([]byte, 200)
	for i := 0; i < 100; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(i*100))
	}

	tests := []struct {
		name          string
		sampleRate    int
		numChannels   int
		bitsPerSample int
	}{
		{"16kHz mono 16-bit", 16000, 1, 16},
		{"24kHz mono 16-bit", 24000, 1, 16},
		{"48kHz mono 16-bit", 48000, 1, 16},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wav := wrapPCMInWAV(pcm, tc.sampleRate, tc.numChannels, tc.bitsPerSample)

			// Check total length
			expectedLen := 44 + len(pcm)
			if len(wav) != expectedLen {
				t.Errorf("len(wav) = %d, want %d", len(wav), expectedLen)
			}

			// Check RIFF header
			if !bytes.Equal(wav[0:4], []byte("RIFF")) {
				t.Errorf("RIFF header = %q, want %q", wav[0:4], "RIFF")
			}

			fileSize := binary.LittleEndian.Uint32(wav[4:8])
			if fileSize != uint32(36+len(pcm)) {
				t.Errorf("file size = %d, want %d", fileSize, 36+len(pcm))
			}

			if !bytes.Equal(wav[8:12], []byte("WAVE")) {
				t.Errorf("WAVE format = %q, want %q", wav[8:12], "WAVE")
			}

			// Check fmt chunk
			if !bytes.Equal(wav[12:16], []byte("fmt ")) {
				t.Errorf("fmt chunk ID = %q, want %q", wav[12:16], "fmt ")
			}

			chunkSize := binary.LittleEndian.Uint32(wav[16:20])
			if chunkSize != 16 {
				t.Errorf("fmt chunk size = %d, want 16", chunkSize)
			}

			audioFormat := binary.LittleEndian.Uint16(wav[20:22])
			if audioFormat != 1 {
				t.Errorf("audio format = %d, want 1 (PCM)", audioFormat)
			}

			numChannels := binary.LittleEndian.Uint16(wav[22:24])
			if int(numChannels) != tc.numChannels {
				t.Errorf("num channels = %d, want %d", numChannels, tc.numChannels)
			}

			sampleRate := binary.LittleEndian.Uint32(wav[24:28])
			if int(sampleRate) != tc.sampleRate {
				t.Errorf("sample rate = %d, want %d", sampleRate, tc.sampleRate)
			}

			byteRate := binary.LittleEndian.Uint32(wav[28:32])
			expectedByteRate := tc.sampleRate * tc.numChannels * tc.bitsPerSample / 8
			if int(byteRate) != expectedByteRate {
				t.Errorf("byte rate = %d, want %d", byteRate, expectedByteRate)
			}

			blockAlign := binary.LittleEndian.Uint16(wav[32:34])
			expectedBlockAlign := tc.numChannels * tc.bitsPerSample / 8
			if int(blockAlign) != expectedBlockAlign {
				t.Errorf("block align = %d, want %d", blockAlign, expectedBlockAlign)
			}

			bitsPerSample := binary.LittleEndian.Uint16(wav[34:36])
			if int(bitsPerSample) != tc.bitsPerSample {
				t.Errorf("bits per sample = %d, want %d", bitsPerSample, tc.bitsPerSample)
			}

			// Check data chunk
			if !bytes.Equal(wav[36:40], []byte("data")) {
				t.Errorf("data chunk ID = %q, want %q", wav[36:40], "data")
			}

			dataSize := binary.LittleEndian.Uint32(wav[40:44])
			if int(dataSize) != len(pcm) {
				t.Errorf("data size = %d, want %d", dataSize, len(pcm))
			}

			// Check PCM data is preserved
			if !bytes.Equal(wav[44:], pcm) {
				t.Errorf("PCM data not preserved correctly")
			}
		})
	}
}

func TestWrapPCMInWAVEmpty(t *testing.T) {
	pcm := []byte{}
	wav := wrapPCMInWAV(pcm, 16000, 1, 16)

	if len(wav) != 44 {
		t.Errorf("len(wav) = %d, want 44 (header only)", len(wav))
	}

	dataSize := binary.LittleEndian.Uint32(wav[40:44])
	if dataSize != 0 {
		t.Errorf("data size = %d, want 0", dataSize)
	}
}

func TestWrapPCMInWAVPreservesData(t *testing.T) {
	// Create specific PCM data pattern
	pcm := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	wav := wrapPCMInWAV(pcm, 24000, 1, 16)

	// Verify PCM data is exactly preserved after header
	if !bytes.Equal(wav[44:], pcm) {
		t.Errorf("PCM data corrupted after wrapping")
	}
}
