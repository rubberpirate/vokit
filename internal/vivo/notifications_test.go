package vivo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestPhoneNotificationBoundsAndUnrelatedMessages(t *testing.T) {
	prefix := "NOTIFY_CLIENT_RECEIVED_NOTIFICATION:"
	valid := prefix + `{"appName":"Test app","title":"Test title","content":"Private test <b>body</b>","pendingIntentId":23}`
	value, matched, err := ParsePhoneNotification([]byte(valid))
	if !matched || err != nil || value.App != "Test app" || value.Body != "Private test <b>body</b>" {
		t.Fatal("notification parse failed")
	}
	for _, raw := range []string{`null`, `{}`, `{"title":23}`, `{"title":"\u0000"}`, `{"appName":"` + strings.Repeat("a", 257) + `"}`, `{"title":"` + strings.Repeat("a", 1025) + `"}`, `{"content":"` + strings.Repeat("a", 8193) + `"}`} {
		if _, matched, err := ParsePhoneNotification([]byte(prefix + raw)); !matched || err == nil {
			t.Fatal("invalid notification accepted")
		}
	}
	if _, matched, _ := ParsePhoneNotification([]byte(`CLIPBOARD_TEXT:test`)); matched {
		t.Fatal("unrelated message treated as notification")
	}
}

func TestPhoneConfigurationAndNotificationControlDelivery(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			got := make(chan PhoneNotification, 1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_, _, _ = conn.Read(ctx)
				_, data, err := conn.Read(ctx)
				if err != nil || !strings.HasPrefix(string(data), "PC_CONFIG:") {
					t.Error("phone configuration missing")
					return
				}
				var config map[string]any
				if json.Unmarshal(data[len("PC_CONFIG:"):], &config) != nil || config["notify"] != enabled || config["notifyOn"] != enabled || config["clipboard_switch"] != false || config["phoneCall"] != false {
					t.Error("incorrect feature configuration")
				}
				_ = conn.Write(ctx, websocket.MessageText, []byte(`NOTIFY_CLIENT_RECEIVED_NOTIFICATION:{"appName":"Fixture","title":"Fixture title","content":"Fixture body"}`))
				_ = conn.Close(websocket.StatusNormalClosure, "")
			}))
			defer server.Close()
			client, _ := newPhoneClientTrust(server.URL, "", true)
			defer client.Close()
			options := ControlOptions{Configure: true}
			if enabled {
				options.NotificationMessage = func(data []byte) {
					value, matched, err := ParsePhoneNotification(data)
					if matched && err == nil {
						got <- value
					}
				}
			}
			var events []Event
			if err := client.controlFeatures(ctx, &Session{Token: "private-token"}, time.Minute, options, func(event Event) { events = append(events, event) }); err != nil {
				t.Fatal(err)
			}
			if (len(got) == 1) != enabled {
				t.Fatal("notification callback enablement ignored")
			}
			diagnostics, _ := json.Marshal(events)
			if strings.Contains(string(diagnostics), "Fixture") || strings.Contains(string(diagnostics), "private-token") {
				t.Fatal("notification contents leaked into diagnostic events")
			}
		})
	}
}
