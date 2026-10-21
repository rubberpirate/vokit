package vivo

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

type PhoneNotification struct {
	App   string `json:"appName"`
	Title string `json:"title"`
	Body  string `json:"content"`
}

// The near-phone control handler uses this prefix and these fields in the
// vendor renderer's receiveTuoGuanMessage / CREATE_NOTIFICATION path.
func ParsePhoneNotification(data []byte) (PhoneNotification, bool, error) {
	const prefix = "NOTIFY_CLIENT_RECEIVED_NOTIFICATION:"
	var result PhoneNotification
	if !bytes.HasPrefix(data, []byte(prefix)) {
		return result, false, nil
	}
	raw := data[len(prefix):]
	if len(raw) > 64*1024 || !utf8.Valid(raw) || json.Unmarshal(raw, &result) != nil {
		return PhoneNotification{}, true, errors.New("invalid phone notification")
	}
	for _, field := range []struct {
		value string
		limit int
	}{{result.App, 256}, {result.Title, 1024}, {result.Body, 8192}} {
		if len(field.value) > field.limit || strings.ContainsRune(field.value, 0) {
			return PhoneNotification{}, true, errors.New("oversized phone notification")
		}
	}
	if strings.TrimSpace(result.App+result.Title+result.Body) == "" {
		return PhoneNotification{}, true, errors.New("empty phone notification")
	}
	return result, true, nil
}

func pcConfiguration(options ControlOptions) []byte {
	enabled := options.NotificationMessage != nil
	value, _ := json.Marshal(map[string]any{
		"update_place": "all", "call": false, "notify": enabled, "notifyOn": enabled,
		"brower": false, "picture": false, "note_relay": false, "phoneCall": false,
		"verifyCode": false, "clipboard_switch": options.Clipboard != nil,
	})
	return append([]byte("PC_CONFIG:"), value...)
}
