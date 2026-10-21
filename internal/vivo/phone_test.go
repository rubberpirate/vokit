package vivo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestFirstUseConnectionFailsClosedWithoutRedial(t *testing.T) {
	for _, test := range []string{"closed", "oversized", "cancel"} {
		t.Run(test, func(t *testing.T) {
			var mu sync.Mutex
			requests := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				requests++
				mu.Unlock()
				switch test {
				case "closed":
					w.Header().Set("Connection", "close")
					fmt.Fprint(w, `{"code":"0000","data":{"pcSuiteVersionCode":70007,"connBaseVersionCode":3134,"enpVer":1}}`)
				case "oversized":
					fmt.Fprint(w, strings.Repeat("x", maxResponseBytes+1))
				case "cancel":
					cancel()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			client, _ := newPhoneClientTrust(server.URL, "", true)
			defer client.Close()
			_, err := client.Negotiate(ctx, &Session{Token: "secret"})
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatal("unsafe result", err)
			}
			if test == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal("cancel not propagated", err)
			}
			_, err = client.Negotiate(context.Background(), &Session{Token: "secret"})
			if err == nil {
				t.Fatal("redialed a closed first-use session")
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 1 {
				t.Fatalf("reissued request after close: %d", requests)
			}
		})
	}
}

func TestFirstUsePinsActualHTTPConnectionAndReusesItForControl(t *testing.T) {
	var mu sync.Mutex
	connections := make(map[string]bool)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connections[r.RemoteAddr] = true
		mu.Unlock()
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"code":"0000","data":{"pcSuiteVersionCode":70007,"connBaseVersionCode":3134,"enpVer":1}}`)
		case "/base-info":
			fmt.Fprint(w, `{"code":"0000","data":{}}`)
		case "/ws/heart-beat":
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v1.hc.vivo.com.cn"}})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			_, _, _ = conn.Read(r.Context())
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
	}))
	defer server.Close()
	client, err := newPhoneClientTrust(server.URL, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session := &Session{Token: "ephemeral-secret"}
	if _, err := client.Negotiate(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(server.Certificate().Raw)
	if client.CertificatePin() != hex.EncodeToString(digest[:]) {
		t.Fatal("actual token connection was not pinned")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Control(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connections) != 1 {
		t.Fatalf("HTTP/WSS did not reuse one pinned TLS connection: %d", len(connections))
	}
}

func TestPinnedPhoneNegotiatesLinuxWithoutAdvertisingUnimplementedFeatures(t *testing.T) {
	var gotVersion, gotBase bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("newToken") != "ephemeral-secret" || r.Header.Get("X-ES-HTTP-VERSION") != "1" || r.Method != "POST" {
			t.Error("phone auth/protocol headers")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
		}
		if body["token"] != "ephemeral-secret" {
			t.Error("body token missing")
		}
		switch r.URL.Path {
		case "/version":
			gotVersion = true
			if body["pcOsType"] != "linux" || body["pcSuiteVersionCode"] != float64(70007) || r.Header.Get("version") != "7.0.6" {
				t.Error("version contract")
			}
			fmt.Fprint(w, `{"code":"0000","data":{"pcSuiteVersionCode":70007,"connBaseVersionCode":3134,"enpVer":1}}`)
		case "/base-info":
			gotBase = true
			if body["isLogin"] != false || body["isSupVdfs"] != false || body["isPcOsSupportExtScreen"] != false {
				t.Error("advertised unimplemented capabilities")
			}
			fmt.Fprint(w, `{"code":"0000","data":{"mobileDeviceName":"Private Phone","isSupportCall":true,"isSupportPcMode":false,"openid":"private-id","innerRoot":"/storage/emulated/0"}}`)
		default:
			t.Error("unexpected phone request")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	digest := sha256.Sum256(server.Certificate().Raw)
	client, err := newPhoneClient(server.URL, hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Identity: Identity{DeviceID: "generated-id"}, HostName: "Linux laptop", Token: "ephemeral-secret", ConnectionID: "session"}
	capabilities, err := client.Negotiate(context.Background(), s)
	if err != nil || !gotVersion || !gotBase || !capabilities["isSupportCall"] || capabilities["isSupportPcMode"] {
		t.Fatalf("negotiation: %v %#v", err, capabilities)
	}
	if s.DeviceName != "Private Phone" {
		t.Fatal("negotiated phone name missing")
	}
	if s.StorageRoot != "/storage/emulated/0" {
		t.Fatal("negotiated storage root missing")
	}
	b, _ := json.Marshal(capabilities)
	if strings.Contains(string(b), "Private") || strings.Contains(string(b), "private-id") {
		t.Fatal("personal data included in capability summary")
	}
}

func TestPhoneRejectsUnknownOrChangedCertificateBeforeSendingToken(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	for _, pin := range []string{"", strings.Repeat("0", 64)} {
		client, err := newPhoneClient(server.URL, pin)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Negotiate(context.Background(), &Session{Token: "secret"})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("untrusted/unsafe request", err)
		}
	}
	if requests != 0 {
		t.Fatal("token sent before certificate verification")
	}
}

func TestPhoneRejectsUnsupportedVersionAndDoesNotFollowRedirects(t *testing.T) {
	for _, response := range []string{`{"code":"0000","data":{"pcSuiteVersionCode":70006,"connBaseVersionCode":3134,"enpVer":1}}`, `{"code":"0000","data":{"pcSuiteVersionCode":70007,"connBaseVersionCode":3134,"enpVer":2}}`, `{"code":"denied","msg":"private-secret"}`} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) }))
		digest := sha256.Sum256(server.Certificate().Raw)
		client, _ := newPhoneClient(server.URL, hex.EncodeToString(digest[:]))
		_, err := client.Negotiate(context.Background(), &Session{Token: "secret"})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("accepted unsupported or leaked response: %v", err)
		}
		server.Close()
	}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	digest := sha256.Sum256(server.Certificate().Raw)
	client, _ := newPhoneClient(server.URL, hex.EncodeToString(digest[:]))
	if _, err := client.Negotiate(context.Background(), &Session{Token: "secret"}); err == nil || requests != 1 {
		t.Fatal("phone redirect followed or accepted", err)
	}
}

func TestCertificateInspectionDoesNotSendAnHTTPRequest(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++ }))
	defer server.Close()
	digest := sha256.Sum256(server.Certificate().Raw)
	fingerprint, err := inspectCertificate(context.Background(), strings.TrimPrefix(server.URL, "https://"))
	if err != nil || fingerprint != hex.EncodeToString(digest[:]) || requests != 0 {
		t.Fatalf("public metadata probe: %v, HTTP requests: %d", err, requests)
	}
}

func TestControlCancellationClosesBlockedRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(r.Context())
		cancel()
		_, _, _ = conn.Read(r.Context())
	}))
	defer server.Close()
	defer cancel()
	digest := sha256.Sum256(server.Certificate().Raw)
	client, _ := newPhoneClient(server.URL, hex.EncodeToString(digest[:]))
	done := make(chan error, 1)
	go func() { done <- client.Control(ctx, &Session{Token: "ephemeral-secret"}, nil) }()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal("cancel was not propagated", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control failed to stop after cancellation")
	}
}

func TestControlReportsAbruptPeerDisconnect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_, _, _ = conn.Read(r.Context())
		conn.CloseNow()
	}))
	defer server.Close()
	digest := sha256.Sum256(server.Certificate().Raw)
	client, _ := newPhoneClient(server.URL, hex.EncodeToString(digest[:]))
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var last Event
	err := client.Control(ctx, &Session{Token: "secret"}, func(event Event) { last = event })
	if !errors.Is(err, ErrControlDisconnected) || last.Stage != "control_disconnected" || last.Code != "transport_closed" {
		t.Fatalf("abrupt disconnect not distinguished: %v %#v", err, last)
	}
}

func TestControlUsesTokenSubprotocolHeartbeatAndSanitizedEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/heart-beat" || strings.ReplaceAll(r.Header.Get("Sec-WebSocket-Protocol"), " ", "") != "v1.hc.vivo.com.cn,ephemeral-secret" {
			t.Error("control upgrade contract")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v1.hc.vivo.com.cn"}})
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		_, first, err := conn.Read(ctx)
		if err != nil || string(first) != `{"state":"normal"}` {
			done <- fmt.Errorf("initial heartbeat: %s %v", first, err)
			return
		}
		if err = conn.Write(ctx, websocket.MessageText, []byte(`CLIPBOARD_TEXT:private-content`)); err != nil {
			done <- err
			return
		}
		_, second, err := conn.Read(ctx)
		if err != nil || string(second) != `{"state":"normal"}` {
			done <- fmt.Errorf("periodic heartbeat: %v", err)
			return
		}
		done <- nil
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer server.Close()
	digest := sha256.Sum256(server.Certificate().Raw)
	client, _ := newPhoneClient(server.URL, hex.EncodeToString(digest[:]))
	var events []Event
	var mu sync.Mutex
	err := client.control(ctx, &Session{Token: "ephemeral-secret"}, 20*time.Millisecond, func(e Event) { mu.Lock(); defer mu.Unlock(); events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private-content") || !strings.Contains(string(encoded), "CLIPBOARD_TEXT") {
		t.Fatalf("unsafe/missing events: %s", encoded)
	}
}
