package vivo

import (
	"strings"
	"testing"
)

func TestClipboardWireVariantsAndBounds(t *testing.T) {
	for _, test := range []struct{ wire, want string }{
		{"CLIPBOARD_TEXT:Hello नमस्ते\n:a", "Hello नमस्ते\n:a"},
		{`CLIPBOARD_TEXT:"quoted\ntext"`, "quoted\ntext"},
		{`CLIPBOARD_TEXT:{"text":"世界"}`, "世界"},
		{`CLIPBOARD_TEXT:{"value":"hello"}`, "hello"},
		{`CLIPBOARD_TEXT:{"content":"hello"}`, "hello"},
		{`CLIPBOARD_TEXT:{"unknown":"private"}`, `{"unknown":"private"}`},
		{`CLIPBOARD_TEXT:12345`, "12345"},
		{`CLIPBOARD_TEXT:true`, "true"},
		{`CLIPBOARD_TEXT:null`, "null"},
		{`CLIPBOARD_TEXT:[1,2]`, "[1,2]"},
		{"OTHER:private", ""},
	} {
		got, err := clipboardText([]byte(test.wire))
		if err != nil || got != test.want {
			t.Errorf("decode variant failed: %v", err)
		}
	}
	for _, payload := range []string{strings.Repeat("a", MaxClipboardBytes+1), "bad\x00text", string([]byte{0xff})} {
		if _, err := clipboardText([]byte("CLIPBOARD_TEXT:" + payload)); err == nil {
			t.Fatal("unsafe clipboard accepted")
		}
	}
}

func TestClipboardSuppressesEchoButAllowsNewChanges(t *testing.T) {
	var guard clipboardGuard
	if !guard.receive("phone A") || !guard.send("local B") {
		t.Fatal("new text lost")
	}
	if guard.send("phone A") {
		t.Fatal("delayed desktop echo forwarded")
	}
	if guard.receive("local B") {
		t.Fatal("phone echo reapplied")
	}
	if !guard.send("local C") || guard.send("local C") {
		t.Fatal("duplicate change sent")
	}
	if !guard.receive("phone D") || guard.receive("phone D") || guard.send("phone D") {
		t.Fatal("phone echo loop")
	}
	if !guard.send("local C") {
		t.Fatal("copying previous different text suppressed")
	}
}

func TestFilePermissionDoesNotConfuseRecentDeleteSwitch(t *testing.T) {
	for _, wire := range []string{`FILE_MANAGER:{"auth":1}`, `FILE_MANAGER:{"auth":"1"}`} {
		if allowed, ok := filePermission([]byte(wire)); !allowed || !ok {
			t.Fatal("permission not recognized")
		}
	}
	if allowed, ok := filePermission([]byte(`FILE_MANAGER:{"auth":0}`)); allowed || !ok {
		t.Fatal("denial ignored")
	}
	if _, ok := filePermission([]byte(`FILE_MANAGER_SWITCH:{"auth":1,"state":1}`)); ok {
		t.Fatal("recent-delete switch used as permission")
	}
}
