package voice

import (
	"encoding/binary"
)

// wrapPCMInWAV wraps raw PCM audio data in a WAV file header.
// Parameters:
//   - pcm: raw PCM audio bytes (16-bit, mono, little-endian)
//   - sampleRate: sample rate in Hz (e.g., 16000, 24000, 48000)
//   - numChannels: number of audio channels (1 for mono, 2 for stereo)
//   - bitsPerSample: bits per sample (typically 16)
//
// Returns a complete WAV file (44-byte header + PCM data).
func wrapPCMInWAV(pcm []byte, sampleRate, numChannels, bitsPerSample int) []byte {
	byteRate := sampleRate * numChannels * bitsPerSample / 8
	blockAlign := numChannels * bitsPerSample / 8
	dataSize := len(pcm)
	fileSize := 36 + dataSize

	header := make([]byte, 44)

	// RIFF header
	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], uint32(fileSize))
	copy(header[8:12], []byte("WAVE"))

	// fmt chunk
	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16) // chunk size
	binary.LittleEndian.PutUint16(header[20:22], 1)  // audio format (1 = PCM)
	binary.LittleEndian.PutUint16(header[22:24], uint16(numChannels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], uint16(bitsPerSample))

	// data chunk
	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataSize))

	// Combine header and PCM data
	result := make([]byte, 0, 44+len(pcm))
	result = append(result, header...)
	result = append(result, pcm...)

	return result
}
