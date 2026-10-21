package vivo

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// InputEvent contains desktop input in decoded video coordinates. Session IDs
// are supplied by the authenticated media connection, never by the desktop.
type InputEvent struct {
	Kind      string `json:"kind"`
	Action    string `json:"action,omitempty"`
	Buttons   int    `json:"buttons,omitempty"`
	X         int    `json:"x,omitempty"`
	Y         int    `json:"y,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	HScroll   int    `json:"hscroll,omitempty"`
	VScroll   int    `json:"vscroll,omitempty"`
	Text      string `json:"text,omitempty"`
	KeyCode   int    `json:"keycode,omitempty"`
	MetaState int    `json:"metastate,omitempty"`
	Quality   string `json:"quality,omitempty"`
}

type inputPosition struct {
	X      int `json:"point_x"`
	Y      int `json:"point_y"`
	Width  int `json:"screen_width"`
	Height int `json:"screen_height"`
}

// EncodeInput implements the recovered Windows producer and KeyMouse serializer
// contract. The caller must gate input on an authorized, active mirror session.
func EncodeInput(e InputEvent, sessionID int) ([]byte, error) {
	if sessionID < 0 || int64(sessionID) > 1<<31-1 {
		return nil, errors.New("invalid input session")
	}
	position := inputPosition{e.X, e.Y, e.Width, e.Height}
	validPosition := func() bool {
		return e.Width > 0 && e.Width <= 16384 && e.Height > 0 && e.Height <= 16384 && e.X >= 0 && e.X < e.Width && e.Y >= 0 && e.Y < e.Height
	}
	action := func(move bool) (int, bool) {
		switch e.Action {
		case "down":
			return 0, true
		case "up":
			return 1, true
		case "move":
			return 2, move
		}
		return 0, false
	}
	var prefix string
	var payload any
	switch e.Kind {
	case "quality":
		pixels, ok := map[string]string{"480p": "0", "720p": "1", "1080p": "2", "auto": "3"}[e.Quality]
		if !ok {
			return nil, errors.New("unsupported screen quality")
		}
		prefix = "RESOLUTION_SETTINGS:"
		payload = struct {
			SessionID int    `json:"sessionId"`
			Pixels    string `json:"pixels"`
			Feedback  string `json:"feedback"`
			Type      string `json:"type"`
		}{sessionID, pixels, "0", "0"}
	case "pointer":
		a, ok := action(true)
		if !ok || !validPosition() || (e.Buttons != 0 && e.Buttons != 1 && e.Buttons != 2 && e.Buttons != 4) || (e.Action != "move" && e.Buttons == 0) {
			return nil, errors.New("invalid pointer input")
		}
		prefix = "MOUSE_EVENT:"
		payload = struct {
			Action    int           `json:"action"`
			Buttons   int           `json:"buttons"`
			ID        int           `json:"id"`
			Position  inputPosition `json:"position"`
			SessionID int           `json:"sessionId"`
		}{a, e.Buttons, 0, position, sessionID}
	case "scroll":
		if !validPosition() || e.HScroll < -1 || e.HScroll > 1 || e.VScroll < -1 || e.VScroll > 1 || (e.HScroll == 0 && e.VScroll == 0) {
			return nil, errors.New("invalid scroll input")
		}
		ratio := float64(float32(.1))
		if e.VScroll != 0 {
			ratio = float64(float32(.01))
		}
		prefix = "SCROLL_EVENT:"
		payload = struct {
			HScroll   int           `json:"hscroll"`
			VScroll   int           `json:"vscroll"`
			Ratio     float64       `json:"wheel_ratio"`
			Position  inputPosition `json:"position"`
			SessionID int           `json:"sessionId"`
		}{-e.HScroll, e.VScroll, ratio, position, sessionID}
	case "text":
		if e.Text == "" || len(e.Text) > 4096 || !utf8.ValidString(e.Text) || strings.ContainsRune(e.Text, 0) {
			return nil, errors.New("invalid text input")
		}
		prefix = "TEXT_EVENT:"
		payload = struct {
			Cursor    int    `json:"newCursorPosition"`
			Type      int    `json:"textEventType"`
			Text      string `json:"text"`
			SessionID int    `json:"sessionId"`
		}{1, 2, e.Text, sessionID}
	case "key":
		a, ok := action(false)
		// Only navigation codes proven in the shipped converter are enabled.
		if !ok || (e.KeyCode != 3 && e.KeyCode != 4 && e.KeyCode != 187) || e.MetaState != 0 {
			return nil, errors.New("unsupported key input")
		}
		prefix = "KEYCODE_EVENT:"
		payload = struct {
			Action    int `json:"action"`
			KeyCode   int `json:"keycode"`
			MetaState int `json:"metastate"`
			SessionID int `json:"sessionId"`
		}{a, e.KeyCode, 0, sessionID}
	default:
		return nil, errors.New("unsupported input kind")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("input encoding failed")
	}
	return append([]byte(prefix), data...), nil
}
