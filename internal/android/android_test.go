package android

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type invocation struct {
	name   string
	args   []string
	stream bool
}
type fakeRunner struct {
	calls  []invocation
	output string
	err    error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, invocation{name, args, false})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []byte(f.output), f.err
}
func (f *fakeRunner) Stream(ctx context.Context, name string, args ...string) error {
	f.calls = append(f.calls, invocation{name, args, true})
	return f.err
}

func TestParseDevices(t *testing.T) {
	input := "* daemon not running; starting now at tcp:5037\r\n* daemon started successfully\r\nList of devices attached\r\nabc123 device usb:1-2 product:V2244 model:V2244 transport_id:1\n192.168.1.2:5555 unauthorized\nother offline\nblocked no permissions (user in plugdev group; are your udev rules wrong?)\n"
	got, err := ParseDevices(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Properties["model"] != "V2244" || got[1].State != "unauthorized" || got[2].State != "offline" || got[3].State != "no permissions" {
		t.Fatalf("unexpected devices: %#v", got)
	}
	for _, input := range []string{"error: broken adb", "List of devices attached\nbroken"} {
		if _, err := ParseDevices(input); err == nil {
			t.Fatalf("accepted malformed response %q", input)
		}
	}
	got, err = ParseDevices("List of devices attached\n\n")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list: %#v, %v", got, err)
	}
}

func TestTransferArguments(t *testing.T) {
	local := filepath.Join(t.TempDir(), "-a file.txt")
	if err := os.WriteFile(local, []byte("sample"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeRunner{}
	c := Client{Runner: fake}
	if err := c.Transfer(context.Background(), "push", "device-1", local, "/sdcard/My Documents/file.txt"); err != nil {
		t.Fatal(err)
	}
	want := invocation{"adb", []string{"-s", "device-1", "push", local, "/sdcard/My Documents/file.txt"}, false}
	if !reflect.DeepEqual(fake.calls[0], want) {
		t.Fatalf("got %#v, want %#v", fake.calls[0], want)
	}
	if err := c.Transfer(context.Background(), "pull", "[::1]:5555", local, "/sdcard/photo.jpg"); err != nil {
		t.Fatal(err)
	}
	want = invocation{"adb", []string{"-s", "[::1]:5555", "pull", "/sdcard/photo.jpg", local}, false}
	if !reflect.DeepEqual(fake.calls[1], want) {
		t.Fatalf("got %#v, want %#v", fake.calls[1], want)
	}
}

func TestRejectUnsafeTransfersBeforeExecuting(t *testing.T) {
	cases := []struct{ direction, serial, local, remote string }{
		{"push", "", "/tmp/file", "/sdcard/file"},
		{"pull", "-s", "/tmp/file", "/sdcard/file"},
		{"pull", "a;id", "/tmp/file", "/sdcard/file"},
		{"pull", "abc", "/tmp/file", "-rf"},
		{"pull", "abc", "/tmp/file", "/sdcard/a;id"},
		{"pull", "abc", "/tmp/file", "/sdcard/$(id)"},
		{"pull", "abc", "/tmp/file", "/sdcard/../secret"},
		{"pull", "abc", "", "/sdcard/a"},
		{"pull", "abc", "a\nfile", "/sdcard/a"},
		{"push", "abc", filepath.Join(t.TempDir(), "missing"), "/sdcard/a"},
		{"rm", "abc", "/tmp/file", "/sdcard/a"},
	}
	for _, tt := range cases {
		fake := &fakeRunner{}
		err := (Client{Runner: fake}).Transfer(context.Background(), tt.direction, tt.serial, tt.local, tt.remote)
		if err == nil || len(fake.calls) != 0 {
			t.Errorf("accepted unsafe request %#v: %v, calls %#v", tt, err, fake.calls)
		}
	}
}

func TestCameraProbeAndMirror(t *testing.T) {
	fake := &fakeRunner{output: "--video-source=value --no-audio"}
	c := Client{Runner: fake}
	if err := c.Mirror(context.Background(), "abc", true); err != nil {
		t.Fatal(err)
	}
	want := []invocation{{"scrcpy", []string{"--help"}, false}, {"scrcpy", []string{"--serial", "abc", "--video-source=camera", "--no-audio"}, true}}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("unexpected camera commands: %#v", fake.calls)
	}
	fake.calls, fake.output = nil, "old help"
	if err := c.Mirror(context.Background(), "abc", true); err == nil || len(fake.calls) != 1 {
		t.Fatalf("unsupported camera started: %v %#v", err, fake.calls)
	}
	fake.calls = nil
	if err := c.Mirror(context.Background(), "abc", false); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 1 || !fake.calls[0].stream {
		t.Fatalf("mirror not streamed: %#v", fake.calls)
	}
}

func TestCanceledOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Client{Runner: &fakeRunner{}}).Devices(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestOutputCap(t *testing.T) {
	var b cappedBuffer
	in := []byte(strings.Repeat("x", 2*1024*1024))
	if n, err := b.Write(in); n != len(in) || err != nil || b.Len() != 1024*1024 {
		t.Fatalf("incorrect buffer cap: %d %v %d", n, err, b.Len())
	}
}
