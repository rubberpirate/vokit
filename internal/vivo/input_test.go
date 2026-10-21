package vivo

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeResolutionSettings(t *testing.T) {
	for quality, pixels := range map[string]string{"480p": "0", "720p": "1", "1080p": "2", "auto": "3"} {
		data, err := EncodeInput(InputEvent{Kind: "quality", Quality: quality}, 37)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			SessionID              int `json:"sessionId"`
			Pixels, Feedback, Type string
		}
		if !strings.HasPrefix(string(data), "RESOLUTION_SETTINGS:") || json.Unmarshal(data[len("RESOLUTION_SETTINGS:"):], &got) != nil || got.SessionID != 37 || got.Pixels != pixels || got.Feedback != "0" || got.Type != "0" {
			t.Fatal("resolution command contract changed")
		}
	}
	for _, quality := range []string{"", "4k", "1", "720P"} {
		if _, err := EncodeInput(InputEvent{Kind: "quality", Quality: quality}, 0); err == nil {
			t.Fatal("unsupported quality accepted")
		}
	}
}

func TestEncodeInputPointerProducerMapping(t *testing.T) {
	for action, wire := range map[string]int{"down": 0, "up": 1, "move": 2} {
		t.Run(action, func(t *testing.T) {
			data, err := EncodeInput(InputEvent{Kind: "pointer", Action: action, Buttons: 1, X: 359, Y: 779, Width: 720, Height: 1560}, 19)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Action, Buttons, ID int
				SessionID           int `json:"sessionId"`
				Position            struct {
					X      int `json:"point_x"`
					Y      int `json:"point_y"`
					Width  int `json:"screen_width"`
					Height int `json:"screen_height"`
				}
			}
			if !strings.HasPrefix(string(data), "MOUSE_EVENT:") {
				t.Fatal("wrong prefix")
			}
			if err := json.Unmarshal(data[len("MOUSE_EVENT:"):], &got); err != nil {
				t.Fatal(err)
			}
			if got.Action != wire || got.Buttons != 1 || got.ID != 0 || got.SessionID != 19 || got.Position.X != 359 || got.Position.Y != 779 || got.Position.Width != 720 || got.Position.Height != 1560 {
				t.Fatalf("unexpected pointer payload: %+v", got)
			}
		})
	}
}

func TestEncodeInputScrollAndText(t *testing.T) {
	data, err := EncodeInput(InputEvent{Kind: "scroll", X: 10, Y: 20, Width: 720, Height: 1560, HScroll: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		HScroll, VScroll int
		Ratio            float64 `json:"wheel_ratio"`
	}
	if err := json.Unmarshal(data[len("SCROLL_EVENT:"):], &got); err != nil {
		t.Fatal(err)
	}
	if got.HScroll != -1 || got.VScroll != 0 || got.Ratio != float64(float32(.1)) {
		t.Fatalf("unexpected horizontal scroll: %+v", got)
	}
	data, err = EncodeInput(InputEvent{Kind: "scroll", X: 10, Y: 20, Width: 720, Height: 1560, VScroll: -1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data[len("SCROLL_EVENT:"):], &got); err != nil {
		t.Fatal(err)
	}
	if got.HScroll != 0 || got.VScroll != -1 || got.Ratio != float64(float32(.01)) {
		t.Fatalf("unexpected vertical scroll: %+v", got)
	}
	data, err = EncodeInput(InputEvent{Kind: "text", Text: "नमस्ते \"世界\"\n"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	var text struct {
		Text    string
		Cursor  int `json:"newCursorPosition"`
		Type    int `json:"textEventType"`
		Session int `json:"sessionId"`
	}
	if err := json.Unmarshal(data[len("TEXT_EVENT:"):], &text); err != nil {
		t.Fatal(err)
	}
	if text.Text != "नमस्ते \"世界\"\n" || text.Cursor != 1 || text.Type != 2 || text.Session != 7 {
		t.Fatal("text contract changed")
	}
}

func TestEncodeInputNavigation(t *testing.T) {
	for _, key := range []int{3, 4, 187} {
		for action, wire := range map[string]int{"down": 0, "up": 1} {
			data, err := EncodeInput(InputEvent{Kind: "key", Action: action, KeyCode: key}, 0)
			if err != nil {
				t.Fatal(err)
			}
			var got struct{ Action, KeyCode, MetaState int }
			if err := json.Unmarshal(data[len("KEYCODE_EVENT:"):], &got); err != nil {
				t.Fatal(err)
			}
			if got.Action != wire || got.KeyCode != key || got.MetaState != 0 {
				t.Fatalf("unexpected key: %+v", got)
			}
		}
	}
}

func TestEncodeInputRejectsInvalid(t *testing.T) {
	base := InputEvent{Kind: "pointer", Action: "down", Buttons: 1, Width: 720, Height: 1560}
	cases := []InputEvent{{}, {Kind: "pointer", Action: "cancel", Width: 1, Height: 1}, {Kind: "text", Text: ""}, {Kind: "text", Text: string([]byte{0xff})}, {Kind: "text", Text: "a\x00b"}, {Kind: "text", Text: strings.Repeat("x", 4097)}, {Kind: "key", Action: "down", KeyCode: 26}, {Kind: "key", Action: "move", KeyCode: 3}, {Kind: "key", Action: "down", KeyCode: 3, MetaState: 1}, {Kind: "scroll", Width: 1, Height: 1}, {Kind: "scroll", Width: 1, Height: 1, HScroll: 120}}
	for _, mutate := range []func(*InputEvent){func(e *InputEvent) { e.X = -1 }, func(e *InputEvent) { e.X = e.Width }, func(e *InputEvent) { e.Y = e.Height }, func(e *InputEvent) { e.Width = 0 }, func(e *InputEvent) { e.Width = 16385 }, func(e *InputEvent) { e.Buttons = 3 }, func(e *InputEvent) { e.Buttons = 0 }} {
		e := base
		mutate(&e)
		cases = append(cases, e)
	}
	for i, e := range cases {
		if _, err := EncodeInput(e, 0); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	for _, sid := range []int{-1, 1 << 32} {
		if _, err := EncodeInput(base, sid); err == nil {
			t.Errorf("invalid session accepted")
		}
	}
}
