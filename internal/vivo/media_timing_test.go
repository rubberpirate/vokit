package vivo

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func timingHeader(source int64, encode int16) []byte {
	h := append([]byte("FRAME:"), make([]byte, 10)...)
	binary.BigEndian.PutUint64(h[6:14], uint64(source))
	binary.BigEndian.PutUint16(h[14:16], uint16(encode))
	return h
}

func TestMediaClockCalibratesOffsetAndSamples(t *testing.T) {
	var clock MediaClock
	start := time.UnixMilli(1_700_000_000_000)
	if string(clock.Request(start)) != `NTP:{"t1":1700000000000}` {
		t.Fatal("unexpected clock request")
	}
	// Phone runs 10 seconds ahead. Exchange: 20ms outward, 5ms processing,
	// 20ms return. NTP removes server processing from the network RTT.
	reply := []byte(`NTP:{"t1":1700000000000,"t2":1700000010020,"t3":1700000010025}`)
	if !clock.Reply(reply, start.Add(45*time.Millisecond)) {
		t.Fatal("valid exchange rejected")
	}
	if clock.Reply(reply, start.Add(46*time.Millisecond)) {
		t.Fatal("replayed exchange accepted")
	}
	sample := clock.Sample(timingHeader(1_700_000_010_100, -2), start.Add(140*time.Millisecond))
	if !sample.ClockValid || sample.EncodeMS != -2 || sample.NetworkRTTMS != 40 || sample.ArrivalAgeMS == nil || *sample.ArrivalAgeMS != 40 || sample.ArrivalDriftMS == nil || *sample.ArrivalDriftMS != 0 {
		t.Fatalf("unexpected timing: %+v", sample)
	}
	next := clock.Sample(timingHeader(1_700_000_010_200, 17), start.Add(270*time.Millisecond))
	if !next.ClockValid || *next.ArrivalAgeMS != 70 || *next.ArrivalDriftMS != 30 {
		t.Fatalf("unexpected relative timing: %+v", next)
	}
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "1700000") || strings.Contains(string(data), "t1") || strings.Contains(string(data), "offset") {
		t.Fatalf("absolute source clock exposed: %s", data)
	}
}

func TestMediaClockUnknownEpochDriftAndPlausibility(t *testing.T) {
	var clock MediaClock
	start := time.UnixMilli(1_700_000_000_000)
	first := clock.Sample(timingHeader(1000, 8), start)
	if first.ClockValid || first.ArrivalAgeMS != nil || first.ArrivalDriftMS == nil || *first.ArrivalDriftMS != 0 {
		t.Fatalf("uncalibrated clock claimed absolute age: %+v", first)
	}
	next := clock.Sample(timingHeader(1100, 9), start.Add(125*time.Millisecond))
	if next.ArrivalDriftMS == nil || *next.ArrivalDriftMS != 25 {
		t.Fatal("relative timing must not need a common epoch")
	}
	clock.Request(start)
	if !clock.Reply([]byte(`NTP:{"t1":1700000000000,"t2":1000,"t3":1000}`), start.Add(20*time.Millisecond)) {
		t.Fatal("uptime-based phone clock rejected")
	}
	plausible := clock.Sample(timingHeader(1010, 10), start.Add(30*time.Millisecond))
	if !plausible.ClockValid || plausible.ArrivalAgeMS == nil || *plausible.ArrivalAgeMS != 10 {
		t.Fatalf("uptime calibration failed: %+v", plausible)
	}
	for _, source := range []int64{1_000_000, 1_700_000_000_000} {
		bad := clock.Sample(timingHeader(source, 1), start.Add(time.Second))
		if bad.ClockValid || bad.ArrivalAgeMS != nil {
			t.Fatal("implausible cross-clock age accepted")
		}
	}
}

func TestMediaClockRejectsUnmatchedInvalidAndOversizedReplies(t *testing.T) {
	start := time.UnixMilli(100000)
	valid := `NTP:{"t1":100000,"t2":110005,"t3":110005}`
	cases := []struct {
		body  string
		after time.Duration
	}{
		{valid, -time.Millisecond},
		{valid, 60001 * time.Millisecond},
		{`NTP:{"t1":100001,"t2":110005,"t3":110005}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":110005,"t3":110004}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":110005,"t3":110020}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":-1,"t3":0}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":9223372036854775807,"t3":9223372036854775807}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":1.5,"t3":1.5}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":null,"t3":0}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":0}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t1":100000,"t2":0,"t3":0}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"t2":0,"t3":0,"extra":[}`, 10 * time.Millisecond},
		{`NTP:{"t1":100000,"extra":{"t1":100000},"t2":0,"t3":0,"t2":0}`, 10 * time.Millisecond},
		{valid + `{}`, 10 * time.Millisecond},
		{`NTP:` + strings.Repeat(" ", 1025) + valid, 10 * time.Millisecond},
		{strings.TrimPrefix(valid, "NTP:"), 10 * time.Millisecond},
	}
	for i, tc := range cases {
		var clock MediaClock
		clock.Request(start)
		if clock.Reply([]byte(tc.body), start.Add(tc.after)) {
			t.Errorf("case %d accepted", i)
		}
		if !clock.Reply([]byte(valid), start.Add(10*time.Millisecond)) {
			t.Errorf("invalid case %d consumed valid pending request", i)
		}
	}
	var clock MediaClock
	if clock.Reply([]byte(valid), start.Add(10*time.Millisecond)) {
		t.Fatal("unsolicited reply accepted")
	}
	clock.Request(start)
	clock.Request(start.Add(time.Second))
	if clock.Reply([]byte(valid), start.Add(1010*time.Millisecond)) {
		t.Fatal("superseded reply accepted")
	}
}

func TestMediaClockAcceptsExtraReplyFieldsWithoutExposingThem(t *testing.T) {
	start := time.UnixMilli(100000)
	for _, body := range []string{
		`NTP:{"t1":100000,"t2":110005,"t3":110005,"secret":"do not log"}`,
		`NTP:{"extra":{"nested":[null,true,{"t1":"unrelated"}]},"t1":100000,"more":1.5,"t2":110005,"empty":null,"t3":110005}`,
	} {
		var clock MediaClock
		clock.Request(start)
		if !clock.Reply([]byte(body), start.Add(10*time.Millisecond)) {
			t.Fatal("valid known fields with extra peer fields rejected")
		}
		sample := clock.Sample(timingHeader(110010, 3), start.Add(20*time.Millisecond))
		data, err := json.Marshal(sample)
		if err != nil || !sample.ClockValid || sample.NetworkRTTMS != 10 {
			t.Fatal("extra fields prevented calibration")
		}
		for _, private := range []string{"secret", "do not log", "nested", "unrelated", "110005"} {
			if strings.Contains(string(data), private) {
				t.Fatal("peer extra field leaked into timing result")
			}
		}
		if clock.Reply([]byte(body), start.Add(21*time.Millisecond)) {
			t.Fatal("extra fields allowed a replay")
		}
	}
}

func TestMediaClockHeaderBoundsAndConcurrentUse(t *testing.T) {
	var clock MediaClock
	start := time.UnixMilli(100000)
	for _, header := range [][]byte{nil, []byte("FRAME:"), append(timingHeader(10, 1), 0), append([]byte("OTHER:"), make([]byte, 10)...), timingHeader(-1, 1)} {
		got := clock.Sample(header, start)
		if got.ClockValid || got.ArrivalAgeMS != nil || got.ArrivalDriftMS != nil {
			t.Fatal("malformed header produced clock metrics")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				clock.Request(start)
				clock.Reply([]byte(`NTP:{"t1":100000,"t2":110000,"t3":110000}`), start.Add(time.Millisecond))
				clock.Sample(timingHeader(110000, 1), start.Add(time.Millisecond))
			}
		}()
	}
	wg.Wait()
}
