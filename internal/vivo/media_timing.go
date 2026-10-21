package vivo

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strconv"
	"sync"
	"time"
)

// MediaTiming contains durations only. The FRAME source clock, NTP timestamps
// and clock offset remain private to MediaClock.
type MediaTiming struct {
	EncodeMS       int64  `json:"encode_ms"`
	NetworkRTTMS   int64  `json:"network_rtt_ms"`
	ArrivalAgeMS   *int64 `json:"arrival_age_ms,omitempty"`
	ArrivalDriftMS *int64 `json:"arrival_drift_ms,omitempty"`
	ClockValid     bool   `json:"clock_valid"`
}

// MediaClock calibrates the phone's millisecond clock with the source-proven
// NTP:{t1} / NTP:{t1,t2,t3} exchange. Its zero value is ready for use.
// Performance.dll RVAs 0x2aa8a–0x2aaba construct the request; 0x26ac5–0x26ccb
// parse and calculate the four-time exchange. See NATIVE_MEDIA_PROTOCOL.md.
type MediaClock struct {
	mu             sync.Mutex
	pending        bool
	sent           time.Time
	t1             int64
	calibrated     bool
	offsetMS       int64 // Phone clock minus PC clock.
	networkRTTMS   int64
	baselineSet    bool
	baselineDiffMS int64
}

// Bound arithmetic and require exact JSON integer values, including if a peer
// uses a clock epoch other than Unix. This covers all plausible ms clocks.
const maxMediaClockMS int64 = 1<<53 - 1

// Request replaces any outstanding probe and returns a TEXT control payload.
// Invalid local times produce no request and clear the outstanding probe.
func (c *MediaClock) Request(now time.Time) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	t1 := now.UnixMilli()
	c.pending = false
	if t1 < 0 || t1 > maxMediaClockMS {
		return nil
	}
	c.sent, c.t1, c.pending = now, t1, true
	data := strconv.AppendInt([]byte(`NTP:{"t1":`), t1, 10)
	return append(data, '}')
}

// Reply accepts one response matching the latest outstanding request. It does
// not expose peer fields, and rejected responses cannot alter a calibration.
func (c *MediaClock) Reply(data []byte, now time.Time) bool {
	if len(data) > 1024 || !bytes.HasPrefix(data, []byte("NTP:")) {
		return false
	}
	values, ok := parseMediaClockReply(data[len("NTP:"):])
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pending || values[0] != c.t1 {
		return false
	}
	t4 := now.UnixMilli()
	if t4 < 0 || t4 > maxMediaClockMS {
		return false
	}
	elapsed := now.Sub(c.sent).Milliseconds()
	wallElapsed := t4 - c.t1
	// A local wall-clock step would corrupt the four-time offset. Millisecond
	// rounding can differ from a monotonic duration by one millisecond.
	if elapsed < 0 || elapsed > 60000 || wallElapsed < 0 || wallElapsed > 60000 || elapsed-wallElapsed < -1 || elapsed-wallElapsed > 1 {
		return false
	}
	processing := values[2] - values[1]
	rtt := wallElapsed - processing
	if processing < 0 || rtt < 0 || rtt > 60000 {
		return false
	}
	c.offsetMS = ((values[1] - c.t1) + (values[2] - t4)) / 2
	c.networkRTTMS, c.calibrated, c.pending = rtt, true, false
	return true
}

func parseMediaClockReply(data []byte) ([3]int64, bool) {
	var values [3]int64
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return values, false
	}
	seen := uint8(0)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return values, false
		}
		index := -1
		switch key {
		case "t1":
			index = 0
		case "t2":
			index = 1
		case "t3":
			index = 2
		}
		if index < 0 {
			// The shipped parser reads only t1/t2/t3. Phones can append other
			// fields; consume their bounded JSON without retaining their values.
			var ignored json.RawMessage
			if decoder.Decode(&ignored) != nil {
				return values, false
			}
			continue
		}
		if seen&(1<<index) != 0 {
			return values, false
		}
		value, err := decoder.Token()
		number, ok := value.(json.Number)
		if err != nil || !ok {
			return values, false
		}
		parsed, err := number.Int64()
		if err != nil || parsed < 0 || parsed > maxMediaClockMS {
			return values, false
		}
		values[index] = parsed
		seen |= 1 << index
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || seen != 7 {
		return values, false
	}
	_, err = decoder.Token()
	return values, err == io.EOF
}

// Sample interprets the exact sixteen-byte header returned by ParseVideoFrame.
// BE64 is BeginTimePoint_Phone; BE16 is sign-extended EncodeTime_Phone
// (VivoCastSDK RVA 0x1f949; Performance.dll RVAs 0x1d9e0 and 0x1de20).
// Relative arrival drift works without assuming a common clock epoch. Absolute
// age is omitted unless a calibrated value falls within ±five minutes.
func (c *MediaClock) Sample(header []byte, now time.Time) MediaTiming {
	var timing MediaTiming
	if len(header) != 16 || !bytes.Equal(header[:6], []byte("FRAME:")) {
		return timing
	}
	source := binary.BigEndian.Uint64(header[6:14])
	if source > uint64(maxMediaClockMS) {
		return timing
	}
	arrival := now.UnixMilli()
	if arrival < 0 || arrival > maxMediaClockMS {
		return timing
	}
	timing.EncodeMS = int64(int16(binary.BigEndian.Uint16(header[14:16])))
	c.mu.Lock()
	defer c.mu.Unlock()
	difference := arrival - int64(source)
	if !c.baselineSet {
		c.baselineDiffMS, c.baselineSet = difference, true
	}
	drift := difference - c.baselineDiffMS
	timing.ArrivalDriftMS = &drift
	if c.calibrated {
		timing.NetworkRTTMS = c.networkRTTMS
		age := difference + c.offsetMS
		if age >= -300000 && age <= 300000 {
			timing.ArrivalAgeMS, timing.ClockValid = &age, true
		}
	}
	return timing
}
