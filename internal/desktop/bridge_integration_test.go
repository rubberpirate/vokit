package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeHelper(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs("../../build/vivo-desktop-helper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("native helper not built; run make desktop")
	} else if err != nil {
		t.Fatal(err)
	}
	// Never connect these tests to the host clipboard or notification daemon.
	display := os.Getenv("VIVOLINUX_TEST_DISPLAY")
	if display == "" {
		t.Skip("run with a private Xvfb display and VIVOLINUX_TEST_DISPLAY to test the GTK helper")
	}
	t.Setenv("DISPLAY", display)
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("GDK_BACKEND", "x11")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "missing-bus"))
	return path
}

func startNativeHelper(t *testing.T, path string, clipboard bool) *Bridge {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	b, err := Start(ctx, path, clipboard)
	if err != nil {
		t.Fatalf("native GTK helper unavailable: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func TestNativeHelperClipboardProtocol(t *testing.T) {
	b := startNativeHelper(t, nativeHelper(t), true)
	assertNoChange := func() {
		t.Helper()
		select {
		case <-b.Changes():
			t.Fatal("unexpected clipboard change")
		case <-time.After(80 * time.Millisecond):
		}
	}
	assertNoChange()
	for _, text := range []string{"नमस्ते 世界\n\"quoted\":value", strings.Repeat("x", MaxTextBytes)} {
		if err := b.Set(context.Background(), text); err != nil {
			t.Fatalf("native clipboard set failed: %v", err)
		}
		select {
		case got, ok := <-b.Changes():
			if !ok || got != text {
				t.Fatal("native clipboard text did not round trip")
			}
		case <-time.After(time.Second):
			t.Fatal("native clipboard event missing")
		}
	}
	if err := b.Set(context.Background(), strings.Repeat("x", MaxTextBytes)); err != nil {
		t.Fatalf("duplicate set acknowledgment failed: %v", err)
	}
	assertNoChange()
	if b.Notify(context.Background(), "connected") == nil {
		t.Fatal("unavailable private DBus unexpectedly accepted notification")
	}
	if err := b.Set(context.Background(), "still running"); err != nil {
		t.Fatalf("notification failure stopped clipboard helper: %v", err)
	}
	select {
	case <-b.Changes():
	case <-time.After(time.Second):
		t.Fatal("clipboard stopped after notification failure")
	}
	b.Close()
	select {
	case _, ok := <-b.Changes():
		if ok {
			t.Fatal("clipboard channel remains open after helper shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("helper shutdown did not close clipboard channel")
	}
}

func TestNativeHelperNotificationsOnlyWithoutDisplay(t *testing.T) {
	path := nativeHelper(t)
	t.Setenv("QT_QPA_PLATFORM", "nonexistent-platform-for-notifications-test")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	b := startNativeHelper(t, path, false)
	if b.Set(context.Background(), "must not reach clipboard") == nil {
		t.Fatal("notification-only helper accepted clipboard operation")
	}
	if b.Notify(context.Background(), "disconnected") == nil {
		t.Fatal("unavailable private DBus unexpectedly accepted notification")
	}
	select {
	case <-b.Changes():
		t.Fatal("notification-only helper emitted clipboard event")
	default:
	}
}
