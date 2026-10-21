package service

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMediaRequestQualityValidation(t *testing.T) {
	for _, r := range []MediaRequest{{Source: "screen"}, {Source: "camera", Facing: "rear"}, {Source: "camera", Facing: "front", Quality: "720p"}, {Source: "camera", Facing: "rear", Quality: "1080p"}, {Source: "camera", Facing: "rear", AudioProbe: true}, {Source: "camera", Facing: "rear", Microphone: true}} {
		if !validMediaRequest(r) {
			t.Fatal("valid source rejected")
		}
	}
	for _, r := range []MediaRequest{{Source: "screen", Quality: "720p"}, {Source: "screen", Facing: "front"}, {Source: "camera", Quality: "720p"}, {Source: "camera", Facing: "rear", Quality: "4k"}, {Source: "camera", Facing: "other"}, {Source: "other"}, {Source: "screen", AudioProbe: true}, {Source: "screen", Microphone: true}} {
		if validMediaRequest(r) {
			t.Fatal("invalid source accepted")
		}
	}
}

func TestMediaPrivateDuplexAndDisconnectCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	socket := filepath.Join(dir, "media.sock")
	ready := make(chan struct{})
	finished := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- ServeMedia(ctx, socket, func(ctx context.Context, r MediaRequest, c *MediaChannel) error {
			defer close(finished)
			if r.Source != "screen" {
				t.Error("source changed")
			}
			if err := c.Info(map[string]int{"width": 720}); err != nil {
				return err
			}
			if _, err := c.Write([]byte{0, 0, 0, 1, 0x67}); err != nil {
				return err
			}
			close(ready)
			select {
			case input := <-c.Input:
				var v map[string]string
				if json.Unmarshal(input, &v) != nil || v["kind"] != "pointer" {
					t.Error("input changed")
				}
			case <-ctx.Done():
				return ctx.Err()
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	var conn net.Conn
	var err error
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		conn, err = net.Dial("unix", socket)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	io.WriteString(conn, "{\"source\":\"screen\"}\n")
	reader := bufio.NewReader(conn)
	for _, kind := range []byte{MediaInfo, MediaVideo} {
		var h [5]byte
		if _, err := io.ReadFull(reader, h[:]); err != nil {
			t.Fatal(err)
		}
		if h[0] != kind || binary.BigEndian.Uint32(h[1:]) > MaxMediaPacket {
			t.Fatal("bad media framing")
		}
		body := make([]byte, binary.BigEndian.Uint32(h[1:]))
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatal(err)
		}
	}
	<-ready
	io.WriteString(conn, "{\"kind\":\"pointer\"}\n")
	conn.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("closing viewer did not cancel media")
	}
	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("media socket is not private")
	}
	cancel()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("media server did not stop")
	}
}

func TestMediaSeparateScreenAndCameraConsumers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "media.sock")
	entered := make(chan string, 4)
	done := make(chan error, 1)
	go func() {
		done <- ServeMedia(ctx, socket, func(ctx context.Context, r MediaRequest, c *MediaChannel) error {
			entered <- r.Source
			if err := c.Info(map[string]bool{"ready": true}); err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	open := func(source string) net.Conn {
		t.Helper()
		var c net.Conn
		var err error
		for until := time.Now().Add(time.Second); time.Now().Before(until); {
			c, err = net.Dial("unix", socket)
			if err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		r := MediaRequest{Source: source}
		if source == "camera" {
			r.Facing = "rear"
			r.Microphone = true
		}
		if err := json.NewEncoder(c).Encode(r); err != nil {
			t.Fatal(err)
		}
		return c
	}
	readKind := func(c net.Conn) byte {
		t.Helper()
		var h [5]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			t.Fatal(err)
		}
		size := binary.BigEndian.Uint32(h[1:])
		if size > MaxMediaPacket {
			t.Fatal("unbounded record")
		}
		if _, err := io.CopyN(io.Discard, c, int64(size)); err != nil {
			t.Fatal(err)
		}
		return h[0]
	}
	screen := open("screen")
	defer screen.Close()
	if readKind(screen) != MediaInfo {
		t.Fatal("screen unavailable")
	}
	camera := open("camera")
	defer camera.Close()
	if readKind(camera) != MediaInfo {
		t.Fatal("camera blocked by screen")
	}
	duplicate := open("camera")
	defer duplicate.Close()
	if readKind(duplicate) != MediaStatus {
		t.Fatal("duplicate camera accepted")
	}
	if len(entered) != 2 {
		t.Fatal("unexpected source acquired")
	}
	cancel()
	screen.Close()
	camera.Close()
	duplicate.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("sources did not stop")
	}
}
