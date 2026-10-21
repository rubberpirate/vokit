package service

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vivolinux/internal/android"
)

type testRunner struct{}

func (testRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte("List of devices attached\nabc device model:Test\n"), nil
}
func (testRunner) Stream(context.Context, string, ...string) error {
	panic("service must never launch interactive commands")
}

func TestServiceRoundTripAndShutdown(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "service.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, socket, android.Client{Runner: testRunner{}}) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil && info.Mode().Perm() == 0600 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("service startup: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("socket startup timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v %v", info, err)
	}
	for _, method := range []string{"status", "devices"} {
		result, err := Call(ctx, socket, method)
		if err != nil || !json.Valid(result) {
			t.Fatalf("%s: %s %v", method, result, err)
		}
		if method == "devices" && !strings.Contains(string(result), "abc") {
			t.Fatalf("missing device: %s", result)
		}
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte("{\"method\":\"push\"}\n"))
	var response Response
	if err := json.NewDecoder(conn).Decode(&response); err != nil || response.Error == "" {
		t.Fatalf("write RPC not rejected: %#v %v", response, err)
	}
	_ = conn.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket not removed on shutdown: %v", err)
	}
}

func TestRejectInsecureDirectoryAndExistingSocket(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "service.sock")
	client := android.Client{Runner: testRunner{}}
	if err := Serve(context.Background(), socket, client); err == nil {
		t.Fatal("accepted public directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), socket, client); err == nil {
		t.Fatal("accepted existing socket path")
	}
	content, err := os.ReadFile(socket)
	if err != nil || string(content) != "keep me" {
		t.Fatalf("existing path modified: %q %v", content, err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(filepath.Join(link, "other.sock")); err == nil {
		t.Fatal("accepted symlink directory")
	}
}
