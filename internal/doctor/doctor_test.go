package doctor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

type probeRunner struct{ calls []string }

func (r *probeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name)
	if len(args) != 1 || args[0] != "--help" {
		panic("doctor must only probe help")
	}
	return []byte("--video-source --no-audio"), nil
}

func TestPlatformToolLists(t *testing.T) {
	macTools := toolNames("darwin")
	for _, name := range []string{"brew", "pbcopy", "pbpaste", "open", "ffplay", "qrencode"} {
		if !slices.Contains(macTools, name) {
			t.Errorf("macOS tool list missing %q: %v", name, macTools)
		}
	}
	for _, name := range []string{"systemctl", "wl-copy", "wl-paste", "xclip", "xdg-open", "pactl", "wpctl"} {
		if slices.Contains(macTools, name) {
			t.Errorf("Linux-only tool %q in macOS list", name)
		}
	}
	linuxTools := toolNames("linux")
	for _, name := range []string{"systemctl", "wl-copy", "wl-paste", "xclip", "xdg-open", "pactl", "wpctl", "qrencode", "ffplay"} {
		if !slices.Contains(linuxTools, name) {
			t.Errorf("Linux tool list changed; missing %q", name)
		}
	}
}

func TestDarwinReportAvoidsLinuxUSBProbe(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin environment check")
	}
	dir := t.TempDir()
	for _, name := range []string{"brew", "pbcopy", "pbpaste", "ffplay", "qrencode"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("unused"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("SSH_TTY", "")
	t.Setenv("SSH_CONNECTION", "")
	r := Collect(context.Background(), &probeRunner{})
	if r.Session != "macos" || len(r.USBDevices) != 0 {
		t.Fatalf("incorrect macOS environment report: %#v", r)
	}
	if !r.Capabilities["homebrew_available"] || !r.Capabilities["macos_clipboard_tooling"] || !r.Capabilities["native_qr_tooling"] || !r.Capabilities["native_video_preview_tooling"] {
		t.Fatalf("installed macOS tools not reported: %#v", r.Capabilities)
	}
	if r.Capabilities["clipboard_sync_implemented"] || r.Capabilities["virtual_webcam_implemented"] {
		t.Fatalf("macOS preview must not claim Linux-only features: %#v", r.Capabilities)
	}
}
func (r *probeRunner) Stream(context.Context, string, ...string) error {
	panic("doctor must not start an interactive process")
}

func TestDoctorDetectsToolsWithoutStartingADB(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux session and clipboard checks")
	}
	dir := t.TempDir()
	for _, name := range []string{"adb", "scrcpy", "wl-copy", "wl-paste"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("unused"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", "")
	runner := &probeRunner{}
	r := Collect(context.Background(), runner)
	if r.Session != "wayland" || !r.Capabilities["adb_file_transfer"] || !r.Capabilities["screen_mirroring"] || !r.Capabilities["scrcpy_camera_option"] || !r.Capabilities["wayland_clipboard_tooling"] {
		t.Fatalf("incorrect capability report: %#v", r)
	}
	if !r.Capabilities["vivo_proprietary_pairing"] || !r.Capabilities["clipboard_sync_implemented"] || !r.Capabilities["virtual_webcam_implemented"] || r.Capabilities["native_qr_tooling"] || r.Capabilities["native_video_preview_tooling"] {
		t.Fatal("implementation availability confused with installed prerequisites")
	}
	if len(runner.calls) != 1 || runner.calls[0] != "scrcpy" {
		t.Fatalf("doctor performed phone action: %#v", runner.calls)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("WAYLAND_DISPLAY", "")
	r = Collect(context.Background(), runner)
	if r.Session != "headless" || r.Capabilities["screen_mirroring"] || r.Capabilities["adb_file_transfer"] {
		t.Fatalf("missing tools misreported: %#v", r)
	}
}
