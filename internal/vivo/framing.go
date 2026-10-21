package vivo

import (
	"bytes"
	"errors"
)

// MaxVideoFrameBytes bounds one media WebSocket message before decoding.
const MaxVideoFrameBytes = 16 << 20

// ParseVideoFrame returns borrowed header and Annex-B payload slices from a
// negotiated CLIENT_VIDEO message. The ten bytes after FRAME: stay opaque.
// VivoCastSDK.dll (SHA256 15a31077...) RVA 0x21333..0x21473 checks FRAME:,
// skips sixteen bytes, then checks the four-byte Annex-B start code.
// Audio uses the client channel; this header has no established type field.
func ParseVideoFrame(data []byte) (header, annexB []byte, err error) {
	if len(data) < 21 || len(data) > MaxVideoFrameBytes {
		return nil, nil, errors.New("video frame size out of bounds")
	}
	if !bytes.Equal(data[:6], []byte("FRAME:")) {
		return nil, nil, errors.New("invalid video frame prefix")
	}
	if !bytes.Equal(data[16:20], []byte{0, 0, 0, 1}) {
		return nil, nil, errors.New("video frame is not Annex-B")
	}
	return data[:16], data[16:], nil
}
