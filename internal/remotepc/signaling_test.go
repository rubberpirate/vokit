package remotepc

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestSignalingTLSContractHeartbeatAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ping := make(chan map[string]json.RawMessage, 1)
	serverDone := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		if r.URL.Path != "/ws/v1/servicename/remote-desktop/id/local-123" || r.Header.Get("Origin") != "https://wwww.vivo.xyz" || r.Header.Get("deviceToken") != "synthetic-token" || len(r.Header.Values("openId")) != 1 || r.Header.Get("openId") != "" {
			t.Error("incorrect signaling URL or authentication headers")
		}
		wantQuery := map[string]string{"controlType": "0", "retry": "0", "pcProcessId": strconv.Itoa(os.Getpid()), "fileTransport": "0", "needAccountAuth": "0", "p2pSecurity": "1"}
		if len(r.URL.Query()) != len(wantQuery) {
			t.Error("unexpected signaling capabilities")
		}
		for name, want := range wantQuery {
			if r.URL.Query().Get(name) != want {
				t.Errorf("incorrect query field %s", name)
			}
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"wwww.vivo.xyz"}})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		serverCtx, stop := context.WithTimeout(ctx, 8*time.Second)
		defer stop()
		for _, message := range []string{
			`{"action":"tokenValidation","timeStamp":"1792650625932","version":"v1","msgId":"synthetic-auth","data":""}`,
			`{"action":"open","version":"v1","targetDeviceNo":"local-123","sourceDeviceNo":"phone-123","connectionId":"session-123","data":"phone-123","ability":{"passwordType":1,"dcHeartBeat":1}}`,
		} {
			if err := conn.Write(serverCtx, websocket.MessageText, []byte(message)); err != nil {
				t.Error(err)
				return
			}
		}
		for {
			_, raw, err := conn.Read(serverCtx)
			if err != nil {
				return
			}
			var envelope map[string]json.RawMessage
			if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 8 {
				t.Error("incorrect signaling envelope shape")
				return
			}
			if string(envelope["action"]) == `"ping"` {
				ping <- envelope
				// Production pongs retain the outgoing ping's phone target.
				pong := maps.Clone(envelope)
				pong["action"] = json.RawMessage(`"pong"`)
				raw, err := json.Marshal(pong)
				if err != nil || conn.Write(serverCtx, websocket.MessageText, raw) != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	s, err := dialSignaling(ctx, Region{WebsocketDomain: strings.Replace(server.URL, "https://", "wss://", 1)}, Device{DeviceNo: "local-123", DeviceToken: "synthetic-token"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, action := range []string{"tokenValidation", "open"} {
		message, err := s.Read(ctx)
		if err != nil || message.Action != action {
			t.Fatalf("read %s: %v", action, err)
		}
		if action == "open" && (message.Ability.PasswordType != 1 || message.Ability.DCHeartBeat != 1 || string(message.Data) != `"phone-123"`) {
			t.Fatal("lost typed open fields")
		}
	}
	if err := s.Send(ctx, SignalMessage{Action: "sipStatus", Data: json.RawMessage(`{"status":1}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case envelope := <-ping:
		for field, want := range map[string]string{"action": `"ping"`, "sourceDeviceNo": `"local-123"`, "targetDeviceNo": `"phone-123"`, "connectionId": `"session-123"`, "version": `"v1"`, "data": `""`} {
			if string(envelope[field]) != want {
				t.Errorf("heartbeat field %s differs", field)
			}
		}
		var timestamp int64
		var id string
		if json.Unmarshal(envelope["timeStamp"], &timestamp) != nil || time.Since(time.UnixMilli(timestamp)) > time.Second || json.Unmarshal(envelope["msgId"], &id) != nil || len(id) < 36 {
			t.Fatal("missing heartbeat identity/timestamp")
		}
	case <-time.After(7 * time.Second):
		t.Fatal("missing four-second JSON heartbeat")
	}
	if message, err := s.Read(ctx); err != nil || message.Action != "pong" || message.TargetDeviceNo != "phone-123" {
		t.Fatalf("reflected gateway heartbeat disconnected the session: %v", err)
	}
	s.mu.Lock()
	peer, session, authed := s.peer, s.session, s.authed
	s.mu.Unlock()
	if peer != "phone-123" || session != "session-123" || !authed {
		t.Fatal("heartbeat changed the authenticated session")
	}
	cancel()
	readCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if _, err := s.Read(readCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("lifetime cancellation did not stop signaling: %v", err)
	}
	select {
	case <-serverDone:
	case <-readCtx.Done():
		t.Fatal("cancellation left the socket open")
	}
}

func TestSignalingTimestampFormats(t *testing.T) {
	for _, timestamp := range []string{`1792650625932`, `"1792650625932"`} {
		var message SignalMessage
		if err := json.Unmarshal([]byte(`{"action":"tokenValidation","timeStamp":`+timestamp+`}`), &message); err != nil || message.TimeStamp != 1792650625932 || message.Action != "tokenValidation" {
			t.Fatalf("gateway timestamp was rejected: %v", err)
		}
	}
	for _, timestamp := range []string{`true`, `{}`, `1.5`, `-1`, `"sensitive-invalid-value"`, `"9223372036854775808"`} {
		var message SignalMessage
		err := json.Unmarshal([]byte(`{"action":"tokenValidation","timeStamp":`+timestamp+`}`), &message)
		if err == nil || strings.Contains(err.Error(), "sensitive-invalid-value") {
			t.Fatal("malformed timestamp accepted or included in an error")
		}
	}
}

func TestSignalingRejectsSpoofedAndUnsupportedOpen(t *testing.T) {
	for name, payload := range map[string]string{
		"before token validation": `{"action":"open","connectionId":"session-123","data":"phone-123"}`,
		"wrong target":            `{"action":"open","targetDeviceNo":"other-device","connectionId":"session-123","data":"phone-123"}`,
		"wrong command target":    `{"action":"msg","targetDeviceNo":"other-device","data":{"type":"key"}}`,
		"spoofed source":          `{"action":"open","sourceDeviceNo":"other-phone","connectionId":"session-123","data":"phone-123"}`,
		"self peer":               `{"action":"open","connectionId":"session-123","data":"local-123"}`,
		"no connection ID":        `{"action":"open","data":"phone-123"}`,
		"object peer":             `{"action":"open","connectionId":"session-123","data":{"deviceNo":"phone-123","deviceToken":"sensitive-token"}}`,
		"file request":            `{"action":"open","connectionType":1,"connectionId":"session-123","data":"phone-123"}`,
		"invalid json":            `{"deviceToken":"sensitive-token"`,
		"oversized":               `{"action":"msg","data":"` + strings.Repeat("x", maxSignalBytes) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"wwww.vivo.xyz"}})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				if name != "before token validation" {
					conn.Write(ctx, websocket.MessageText, []byte(`{"action":"tokenValidation"}`))
				}
				conn.Write(ctx, websocket.MessageText, []byte(payload))
				conn.Read(ctx)
			}))
			defer server.Close()
			s, err := dialSignaling(ctx, Region{WebsocketDomain: strings.Replace(server.URL, "https://", "wss://", 1)}, Device{DeviceNo: "local-123", DeviceToken: "synthetic-token"}, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if name != "before token validation" {
				if _, err := s.Read(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Read(ctx); err == nil || strings.Contains(err.Error(), "sensitive-token") {
				t.Fatal("accepted unsafe message or disclosed payload")
			}
		})
	}
}

func TestSignalingRejectsUnsafeDialAndSend(t *testing.T) {
	for _, region := range []Region{
		{WebsocketDomain: "ws://example.test"}, {WebsocketDomain: "wss://user:secret@example.test"},
		{WebsocketDomain: "wss://example.test/extra"}, {WebsocketDomain: "wss://example.test?token=secret"},
	} {
		if _, err := DialSignaling(context.Background(), region, Device{DeviceNo: "local-123", DeviceToken: "synthetic-token"}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("accepted unsafe signaling URL or leaked credentials")
		}
	}
	s := &Signaling{deviceNo: "local-123"}
	if err := s.Send(context.Background(), SignalMessage{Action: "ping", SourceDeviceNo: "another-device"}); err == nil {
		t.Fatal("allowed local identity spoofing")
	}
	if err := s.Send(context.Background(), SignalMessage{Action: "msg", Data: json.RawMessage(strings.Repeat("x", maxSignalBytes+1))}); err == nil {
		t.Fatal("allowed oversized output")
	}
}

func TestSignalingEndSessionResetsPeerRouting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	serverDone := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"wwww.vivo.xyz"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		write := func(raw string) { conn.Write(ctx, websocket.MessageText, []byte(raw)) }
		read := func() SignalMessage {
			_, raw, err := conn.Read(ctx)
			var message SignalMessage
			if err != nil || json.Unmarshal(raw, &message) != nil {
				t.Errorf("cannot read session envelope: %v", err)
			}
			return message
		}
		write(`{"action":"tokenValidation"}`)
		write(`{"action":"open","connectionId":"old-session","data":"old-phone"}`)
		message := read()
		if message.Action != "closeConnection" || message.TargetDeviceNo != "old-phone" || message.ConnectionID != "old-session" || string(message.Data) != `{"type":"closeConnection"}` {
			t.Error("session close envelope differs from recovered contract")
		}
		message = read()
		if message.Action != "ping" || message.TargetDeviceNo != "" || message.ConnectionID != "" {
			t.Error("heartbeat retained a closed session")
		}
		write(`{"action":"close","connectionId":"old-session","sourceDeviceNo":"old-phone"}`)
		write(`{"action":"open","connectionId":"new-session","data":"new-phone"}`)
		write(`{"action":"close","connectionId":"old-session","sourceDeviceNo":"old-phone"}`)
		write(`{"action":"close","connectionId":"new-session","sourceDeviceNo":"other-phone"}`)
		message = read()
		if message.Action != "ping" || message.TargetDeviceNo != "new-phone" || message.ConnectionID != "new-session" {
			t.Error("unrelated close cleared current session routing")
		}
	}))
	defer server.Close()
	s, err := dialSignaling(ctx, Region{WebsocketDomain: strings.Replace(server.URL, "https://", "wss://", 1)}, Device{DeviceNo: "local-123", DeviceToken: "synthetic-token"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if _, err := s.Read(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndSession(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx); err != nil {
		t.Fatalf("closing an empty session should be harmless: %v", err)
	}
	if err := s.Send(ctx, SignalMessage{Action: "ping"}); err != nil {
		t.Fatal(err)
	}
	if message, err := s.Read(ctx); err != nil || message.Action != "close" {
		t.Fatalf("late close after SIP BYE should be harmless: %v", err)
	}
	if message, err := s.Read(ctx); err != nil || message.ConnectionID != "new-session" {
		t.Fatalf("new peer was not accepted after reset: %v", err)
	}
	if message, err := s.Read(ctx); err != nil || message.ConnectionID != "old-session" {
		t.Fatalf("late close from previous session should be harmless: %v", err)
	}
	if _, err := s.Read(ctx); err == nil {
		t.Fatal("unrelated close was accepted")
	}
	if err := s.Send(ctx, SignalMessage{Action: "ping"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("session reset exchange did not finish")
	}
}
