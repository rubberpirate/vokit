package vivo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxClipboardBytes = 64 * 1024

type Clipboard interface {
	Changes() <-chan string
	Set(context.Context, string) error
}

type ControlOptions struct {
	Clipboard Clipboard
	// Supplying a permission channel requests FILE_MANAGER access once connected.
	FilePermission         chan<- bool
	MirrorPermission       chan<- bool
	FilePermissionState    func(bool)
	FilePermissionRequests <-chan struct{}
	Connected              func()
	MainRequests           <-chan []byte
	CameraMessage          func([]byte)
	MirrorPermissionState  func(bool)
}

func mirrorPermission(data []byte) (bool, bool) {
	const prefix = "res_authrity"
	if !strings.HasPrefix(string(data), prefix) {
		return false, false
	}
	raw := strings.TrimPrefix(string(data[len(prefix):]), ":")
	var reply struct {
		Auth json.RawMessage `json:"auth"`
	}
	if json.Unmarshal([]byte(raw), &reply) != nil || len(reply.Auth) == 0 {
		return false, false
	}
	return string(reply.Auth) == "1" || string(reply.Auth) == `"1"`, true
}

func clipboardText(data []byte) (string, error) {
	const prefix = "CLIPBOARD_TEXT:"
	if !strings.HasPrefix(string(data), prefix) {
		return "", nil
	}
	raw := data[len(prefix):]
	if !utf8.Valid(raw) || len(raw) > MaxClipboardBytes*6+1024 {
		return "", errors.New("invalid or oversized clipboard message")
	}
	text := string(raw)
	if json.Valid(raw) {
		var value any
		_ = json.Unmarshal(raw, &value)
		switch value := value.(type) {
		case string:
			text = value
		case map[string]any:
			for _, key := range []string{"text", "value", "content"} {
				if v, ok := value[key].(string); ok && v != "" {
					text = v
					break
				}
			}
		}
	}
	if len(text) > MaxClipboardBytes || strings.ContainsRune(text, 0) {
		return "", errors.New("clipboard text exceeds limit or contains NUL")
	}
	return text, nil
}

// Pending local change events from writes to the desktop must never echo back.
type clipboardGuard struct {
	mu          sync.Mutex
	current     [32]byte
	haveCurrent bool
	pending     map[[32]byte]time.Time
}

func (g *clipboardGuard) receive(text string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	hash := sha256.Sum256([]byte(text))
	if g.haveCurrent && hash == g.current {
		return false
	}
	g.current, g.haveCurrent = hash, true
	if g.pending == nil {
		g.pending = make(map[[32]byte]time.Time)
	}
	for key, expiry := range g.pending {
		if time.Now().After(expiry) {
			delete(g.pending, key)
		}
	}
	if len(g.pending) >= 32 {
		clear(g.pending)
	}
	g.pending[hash] = time.Now().Add(5 * time.Second)
	return true
}

func (g *clipboardGuard) send(text string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if text == "" || len(text) > MaxClipboardBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return false
	}
	hash := sha256.Sum256([]byte(text))
	if expiry, ok := g.pending[hash]; ok {
		delete(g.pending, hash)
		if time.Now().Before(expiry) {
			return false
		}
	}
	if g.haveCurrent && hash == g.current {
		return false
	}
	g.current, g.haveCurrent = hash, true
	return true
}

func filePermission(data []byte) (bool, bool) {
	const prefix = "FILE_MANAGER:"
	if !strings.HasPrefix(string(data), prefix) {
		return false, false
	}
	var reply struct {
		Auth json.RawMessage `json:"auth"`
	}
	if json.Unmarshal(data[len(prefix):], &reply) != nil || len(reply.Auth) == 0 {
		return false, false
	}
	return string(reply.Auth) == "1" || string(reply.Auth) == `"1"`, true
}
