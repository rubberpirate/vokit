package vivo

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type testClipboard struct {
	changes chan string
	applied chan string
}

func (c *testClipboard) Changes() <-chan string { return c.changes }
func (c *testClipboard) Set(ctx context.Context, text string) error {
	c.applied <- text
	c.changes <- text // Simulate the desktop's echo after a programmatic write.
	return nil
}

func TestControlClipboardRoundtripDoesNotEchoOrLogContents(t *testing.T) {
	clip := &testClipboard{changes: make(chan string, 8), applied: make(chan string, 1)}
	clip.changes <- "preexisting private copy while waiting for QR"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, heartbeat, _ := conn.Read(ctx)
		if string(heartbeat) != `{"state":"normal"}` {
			t.Error("no initial heartbeat")
		}
		_, permission, _ := conn.Read(ctx)
		if string(permission) != "FILE_MANAGER:" {
			t.Error("wrong file permission command")
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`FILE_MANAGER:{"auth":1}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`CLIPBOARD_TEXT:{"text":"Phone नमस्ते\n:hello"}`))
		select {
		case got := <-clip.applied:
			if got != "Phone नमस्ते\n:hello" {
				t.Error("wrong desktop text")
			}
		case <-ctx.Done():
			t.Error("desktop write missing")
			return
		}
		clip.changes <- "PC 世界\n:hello"
		_, outbound, err := conn.Read(ctx)
		if err != nil || string(outbound) != "CLIPBOARD_TEXT:PC 世界\n:hello" {
			t.Error("clipboard lost, echoed, or preexisting content sent", err)
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte("CLIPBOARD_TEXT:PC 世界\n:hello"))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}))
	defer server.Close()
	client, err := newPhoneClientTrust(server.URL, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	permissions := make(chan bool, 1)
	var mu sync.Mutex
	var events []Event
	err = client.controlFeatures(ctx, &Session{Token: "secret"}, time.Minute, ControlOptions{Clipboard: clip, FilePermission: permissions}, func(e Event) { mu.Lock(); defer mu.Unlock(); events = append(events, e) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case allowed := <-permissions:
		if !allowed {
			t.Fatal("permission missing")
		}
	default:
		t.Fatal("permission reply missing")
	}
	mu.Lock()
	defer mu.Unlock()
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "hello") || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "private copy") {
		t.Fatal("clipboard contents leaked to events")
	}
	if len(clip.applied) != 0 {
		t.Fatal("phone echo overwritten desktop")
	}
}

func TestFirstUseStreamsDownloadLargerThanJSONLimit(t *testing.T) {
	entry := FileEntry{FileName: "large.bin", SavePath: "/storage/emulated/0/Download/large.bin", FileSize: 700 * 1024}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("newToken") != "secret" {
			t.Error("download not authenticated")
		}
		switch r.URL.Path {
		case "/pc_file_manager/download_info":
			fmt.Fprintf(w, `{"totalCount":1,"totalSize":%d}`, entry.FileSize)
		case "/pc_file_manager/download":
			writer := tar.NewWriter(w)
			_ = writer.WriteHeader(&tar.Header{Name: entry.FileName, Size: entry.FileSize, Typeflag: tar.TypeReg, Mode: 0600})
			_, _ = io.CopyN(writer, strings.NewReader(strings.Repeat("x", int(entry.FileSize))), entry.FileSize)
			_ = writer.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := newPhoneClientTrust(server.URL, "", true)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := client.DownloadFile(ctx, &Session{Token: "secret"}, entry, filepath.Join(t.TempDir(), "download.bin"))
	if err != nil || result.Bytes != entry.FileSize {
		t.Fatalf("stream download failed: %v", err)
	}
}

func TestControlAcceptsFilePermissionRequestAfterConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	requests := make(chan struct{}, 1)
	var access FileAccess
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, _, _ = conn.Read(ctx)
		_, data, err := conn.Read(ctx)
		if err != nil || string(data) != "FILE_MANAGER:" {
			t.Error("user action did not send permission request")
			return
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`FILE_MANAGER:{"auth":1}`))
		_, _, _ = conn.Read(ctx)
	}))
	defer server.Close()
	client, _ := newPhoneClientTrust(server.URL, "", true)
	defer client.Close()
	connected := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- client.ControlWithFeatures(ctx, &Session{Token: "secret"}, ControlOptions{FilePermissionRequests: requests, FilePermissionState: access.Update, Connected: func() { close(connected) }}, nil)
	}()
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("control did not connect")
	}
	if err := access.Ensure(ctx, requests, false); err != nil {
		t.Fatal("on-demand permission failed", err)
	}
	cancel()
	<-done
}
