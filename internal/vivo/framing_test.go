package vivo

import (
	"bytes"
	"testing"
)

func TestParseVideoFrame(t *testing.T) {
	// Synthetic opaque metadata followed by an H.264 SPS NAL start.
	frame := append([]byte("FRAME:\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a"), 0, 0, 0, 1, 0x67, 0x42)
	header, payload, err := ParseVideoFrame(frame)
	if err != nil || !bytes.Equal(header, frame[:16]) || !bytes.Equal(payload, frame[16:]) {
		t.Fatalf("valid video frame rejected: %v", err)
	}
	if &header[0] != &frame[0] || &payload[0] != &frame[16] {
		t.Fatal("frame parser copied borrowed data")
	}
	for _, malformed := range [][]byte{
		nil,
		frame[:15],
		frame[:20],
		append([]byte("OTHER:"), frame[6:]...),
		append(append([]byte(nil), frame[:16]...), 0xff, 0xf1, 0x50, 0x80, 0x00),
		append(append([]byte(nil), frame[:16]...), 0, 0, 1, 0x67, 0x42),
		make([]byte, MaxVideoFrameBytes+1),
	} {
		if _, _, err := ParseVideoFrame(malformed); err == nil {
			t.Fatal("malformed or oversized video frame accepted")
		}
	}
}
