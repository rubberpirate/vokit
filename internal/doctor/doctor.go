package doctor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"vivolinux/internal/android"
)

type Report struct {
	Platform     string            `json:"platform"`
	Architecture string            `json:"architecture"`
	Session      string            `json:"session"`
	Tools        map[string]string `json:"tools"`
	Capabilities map[string]bool   `json:"capabilities"`
	USBDevices   []USBDevice       `json:"usb_device_permissions"`
	Notes        []string          `json:"notes"`
}

type USBDevice struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Writable bool   `json:"writable"`
}

func Collect(ctx context.Context, runner android.Runner) Report {
	r := Report{Platform: runtime.GOOS, Architecture: runtime.GOARCH, Session: "headless", Tools: map[string]string{}, Capabilities: map[string]bool{}, USBDevices: []USBDevice{}, Notes: []string{
		"Native QR/control, clipboard and file adapters are experimental; tool availability does not prove phone support.",
		"Only ADB fallback commands require authorized USB debugging. Doctor does not start ADB or contact phones.",
		"USB permissions describe current device nodes; device authorization and udev rules are not verified.",
		"ADB camera preview requires a compatible scrcpy and Android 12+. Native camera/webcam uses the Qt desktop and a writable v4l2loopback output; phone acceptance remains pending.",
		"Native screen picture and touch work on the tested X300 FE; corrected default 720p preview is user-confirmed smooth. Other devices, quality modes and networks remain unverified.",
	}}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		r.Session = "wayland"
	} else if os.Getenv("DISPLAY") != "" {
		r.Session = "x11"
	}
	for _, name := range []string{"adb", "scrcpy", "systemctl", "wl-copy", "wl-paste", "xclip", "xdg-open", "pactl", "wpctl", "qrencode", "ffplay", "vivo-desktop-helper", "vivo-desktop"} {
		if path, err := exec.LookPath(name); err == nil {
			r.Tools[name] = path
		}
	}
	r.Capabilities["adb_file_transfer"] = r.Tools["adb"] != ""
	r.Capabilities["screen_mirroring"] = r.Tools["adb"] != "" && r.Tools["scrcpy"] != "" && r.Session != "headless"
	r.Capabilities["scrcpy_camera_option"] = false
	if r.Tools["scrcpy"] != "" {
		if out, err := runner.Run(ctx, "scrcpy", "--help"); err == nil {
			r.Capabilities["scrcpy_camera_option"] = strings.Contains(string(out), "--video-source") && strings.Contains(string(out), "--no-audio")
		}
	}
	r.Capabilities["wayland_clipboard_tooling"] = r.Tools["wl-copy"] != "" && r.Tools["wl-paste"] != ""
	r.Capabilities["x11_clipboard_tooling"] = r.Tools["xclip"] != ""
	r.Capabilities["vivo_proprietary_pairing"] = true
	r.Capabilities["native_qr_tooling"] = r.Tools["qrencode"] != ""
	r.Capabilities["clipboard_sync_implemented"] = true
	r.Capabilities["native_video_preview_tooling"] = (r.Tools["vivo-desktop"] != "" || r.Tools["ffplay"] != "") && r.Session != "headless"
	r.Capabilities["virtual_webcam_implemented"] = true
	paths, _ := filepath.Glob("/dev/bus/usb/*/*")
	for _, path := range paths {
		d := USBDevice{Path: path}
		d.Readable = syscall.Access(path, 4) == nil // R_OK, without opening the device.
		d.Writable = syscall.Access(path, 2) == nil // W_OK.
		r.USBDevices = append(r.USBDevices, d)
	}
	return r
}
