package service

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func privateTestSocket(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

func TestPrivateSocketRecoversStaleListener(t *testing.T) {
	socket := privateTestSocket(t, "native.sock")
	old, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	old.SetUnlinkOnClose(false)
	os.Chmod(socket, 0600)
	old.Close()
	listener, cleanup, err := listenPrivate(socket)
	if err != nil {
		t.Fatal("stale socket was not recovered", err)
	}
	if info, _ := os.Stat(socket); info.Mode().Perm() != 0600 {
		t.Fatal("socket permissions are not private")
	}
	if info, _ := os.Stat(socket + ".lock"); info.Mode().Perm() != 0600 || !info.Mode().IsRegular() {
		t.Fatal("lock permissions are not private")
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	listener.Close()
	cleanup()
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatal("socket leaked after cleanup")
	}
	// The lock inode stays stable across successive owners.
	before, _ := os.Stat(socket + ".lock")
	_, cleanup, err = listenPrivate(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	after, _ := os.Stat(socket + ".lock")
	if !os.SameFile(before, after) {
		t.Fatal("lock inode changed")
	}
}

func TestPrivateSocketPreservesActiveAndUnsafePaths(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		socket := privateTestSocket(t, "media.sock")
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		before, _ := os.Lstat(socket)
		if _, _, err := listenPrivate(socket); err == nil {
			t.Fatal("replaced active listener")
		}
		after, _ := os.Lstat(socket)
		if !os.SameFile(before, after) {
			t.Fatal("active socket changed")
		}
	})
	for _, kind := range []string{"file", "socket-symlink", "lock-symlink", "public-lock", "lock-directory"} {
		t.Run(kind, func(t *testing.T) {
			socket := privateTestSocket(t, "service.sock")
			target := filepath.Join(t.TempDir(), "keep")
			os.WriteFile(target, []byte("keep"), 0600)
			switch kind {
			case "file":
				os.WriteFile(socket, []byte("keep"), 0600)
			case "socket-symlink":
				os.Symlink(target, socket)
			case "lock-symlink":
				os.Symlink(target, socket+".lock")
			case "public-lock":
				os.WriteFile(socket+".lock", nil, 0644)
			case "lock-directory":
				os.Mkdir(socket+".lock", 0700)
			}
			if _, _, err := listenPrivate(socket); err == nil {
				t.Fatal("accepted unsafe path")
			}
			data, _ := os.ReadFile(target)
			if string(data) != "keep" {
				t.Fatal("modified symlink target")
			}
		})
	}
}

func TestPrivateSocketConcurrentOwnersAndCleanupIdentity(t *testing.T) {
	socket := privateTestSocket(t, "native.sock")
	start := make(chan struct{})
	release := make(chan struct{})
	var workers sync.WaitGroup
	results := make(chan bool, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, cleanup, err := listenPrivate(socket)
			results <- err == nil
			if err == nil {
				<-release
				cleanup()
			}
		}()
	}
	close(start)
	one, two := <-results, <-results
	if one == two {
		t.Fatal("expected exactly one socket owner")
	}
	close(release)
	workers.Wait()
	_, cleanup, err := listenPrivate(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(socket, socket+".old"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(socket, []byte("replacement"), 0600)
	cleanup()
	data, _ := os.ReadFile(socket)
	if string(data) != "replacement" {
		t.Fatal("cleanup deleted replacement inode")
	}
}

func TestServicesReportReadinessAndReclaimStaleSockets(t *testing.T) {
	for _, kind := range []string{"native", "media"} {
		t.Run(kind, func(t *testing.T) {
			socket := privateTestSocket(t, kind+".sock")
			stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			stale.SetUnlinkOnClose(false)
			stale.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := make(chan error, 1)
			done := make(chan error, 1)
			notify := func(err error) { ready <- err }
			go func() {
				if kind == "native" {
					done <- ServeNativeReady(ctx, socket, func(context.Context, NativeRequest) (any, error) { return true, nil }, notify)
				} else {
					done <- ServeMediaReady(ctx, socket, func(context.Context, MediaRequest, *MediaChannel) error { return nil }, notify)
				}
			}()
			select {
			case err := <-ready:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("no startup result")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("service did not stop")
			}
		})
	}
}
