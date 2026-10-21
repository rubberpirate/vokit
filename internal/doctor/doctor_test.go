package doctor

import (
	"context"
	"os"
	"path/filepath"
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
func (r *probeRunner) Stream(context.Context, string, ...string) error {
	panic("doctor must not start an interactive process")
}

func TestDoctorDetectsToolsWithoutStartingADB(t *testing.T) {
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
