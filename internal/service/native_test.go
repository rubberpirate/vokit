package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startNativeTest(t *testing.T, handler func(context.Context, NativeRequest) (any, error)) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	directory := t.TempDir()
	os.Chmod(directory, 0700)
	socket := filepath.Join(directory, "native.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeNative(ctx, socket, handler) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("native server did not shut down")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Lstat(socket); err == nil && info.Mode().Perm() == 0600 {
			return socket, cancel, done
		}
		if time.Now().After(deadline) {
			t.Fatal("native socket startup timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNativeRoundTripAndSanitizedErrors(t *testing.T) {
	socket, _, _ := startNativeTest(t, func(ctx context.Context, req NativeRequest) (any, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 10*time.Minute {
			t.Error("unbounded native job")
		}
		if req.Method == "files" {
			return nil, errors.New("secret-token private-filename")
		}
		return map[string]bool{"connected": true}, nil
	})
	result, err := CallNative(context.Background(), socket, NativeRequest{Method: "status"})
	if err != nil || string(result) != `{"connected":true}` {
		t.Fatalf("native status: %s %v", result, err)
	}
	_, err = CallNative(context.Background(), socket, NativeRequest{Method: "files", Directory: "/"})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "filename") {
		t.Fatal("native error was not sanitized", err)
	}
	if err := os.Chmod(socket, 0660); err != nil {
		t.Fatal(err)
	}
	if _, err := CallNative(context.Background(), socket, NativeRequest{Method: "status"}); err == nil {
		t.Fatal("accepted insecure socket permissions")
	}
}

func TestNativeStrictBoundedRequests(t *testing.T) {
	var calls atomic.Int32
	socket, _, _ := startNativeTest(t, func(context.Context, NativeRequest) (any, error) {
		calls.Add(1)
		return true, nil
	})
	for _, request := range []string{
		`{"method":"delete"}`,
		`{"method":"status","token":"secret"}`,
		`{"method":"files","directory":"/a/../b"}`,
		`{"method":"download","remote":"/a","output":"relative"}`,
		`{"method":"thumbnail","remote":"/picture","width":2000,"height":128}`,
		`{"method":"preview","remote":"/picture","output":"relative"}`,
		`{"method":"status","width":128}`,
		`{"method":"status"} {"method":"status"}`,
		`{"method":"status","directory":"` + strings.Repeat("a", nativeRequestLimit) + `"}`,
	} {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		conn.Write([]byte(request + "\n"))
		var response Response
		if err := json.NewDecoder(conn).Decode(&response); err != nil || response.Error == "" {
			t.Error("invalid native request was not rejected", err)
		}
		conn.Close()
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached native handler")
	}
	if _, err := CallNative(context.Background(), socket, NativeRequest{Method: "delete"}); err == nil {
		t.Fatal("client accepted unsupported method")
	}
}

func TestNativeUploadRequestValidation(t *testing.T) {
	for _, req := range []NativeRequest{
		{Method: "upload"}, {Method: "upload", Local: "relative", Directory: "/storage"},
		{Method: "upload", Local: "/local", Directory: "/storage/../private"},
		{Method: "upload", Local: "/local", Directory: "/storage", Page: 1},
		{Method: "status", Local: "/local"}, {Method: "download", Remote: "/a", Output: "/b", Local: "/c"},
	} {
		if validNativeRequest(req) {
			t.Fatal("invalid upload RPC accepted")
		}
	}
	socket, _, _ := startNativeTest(t, func(_ context.Context, req NativeRequest) (any, error) {
		if req.Method != "upload" || req.Local != "/local/source" || req.Directory != "/storage" {
			t.Error("upload RPC fields changed")
		}
		return map[string]bool{"uploaded": true}, nil
	})
	if _, err := CallNative(context.Background(), socket, NativeRequest{Method: "upload", Local: "/local/source", Directory: "/storage"}); err != nil {
		t.Fatal("valid upload RPC failed", err)
	}
}

func TestNativeDisconnectAndCancellationStopWork(t *testing.T) {
	started := make(chan struct{}, 2)
	canceled := make(chan struct{}, 2)
	socket, _, _ := startNativeTest(t, func(ctx context.Context, _ NativeRequest) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		return nil, ctx.Err()
	})
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("{\"method\":\"status\"}\n"))
	<-started
	conn.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel native work")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := CallNative(ctx, socket, NativeRequest{Method: "status"})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("client cancellation not propagated", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not stop native work")
	}
}

func TestNativeShutdownClosesIdleAndActiveConnections(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	socket, cancel, _ := startNativeTest(t, func(ctx context.Context, _ NativeRequest) (any, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	idle, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	active, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	active.Write([]byte("{\"method\":\"status\"}\n"))
	<-started
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown left active work running")
	}
	idle.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := idle.Read(data[:]); err == nil {
		t.Fatal("shutdown left idle connection open")
	}
}

func TestNativeRefusesInsecureDirectoryAndExistingPath(t *testing.T) {
	directory := t.TempDir()
	socket := filepath.Join(directory, "native.sock")
	handler := func(context.Context, NativeRequest) (any, error) { return nil, nil }
	os.Chmod(directory, 0755)
	if err := ServeNative(context.Background(), socket, handler); err == nil {
		t.Fatal("accepted insecure directory")
	}
	os.Chmod(directory, 0700)
	os.WriteFile(socket, []byte("keep"), 0600)
	if err := ServeNative(context.Background(), socket, handler); err == nil {
		t.Fatal("accepted existing path")
	}
	if data, _ := os.ReadFile(socket); string(data) != "keep" {
		t.Fatal("removed existing socket path")
	}
}

func TestNativeConnectionLimit(t *testing.T) {
	started := make(chan struct{}, 16)
	socket, cancel, _ := startNativeTest(t, func(ctx context.Context, _ NativeRequest) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	for i := 0; i < 16; i++ {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		conn.Write([]byte("{\"method\":\"status\"}\n"))
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("handler did not start")
		}
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := CallNative(ctx, socket, NativeRequest{Method: "status"}); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("excess connection was not immediately refused", err)
	}
	cancel()
}
