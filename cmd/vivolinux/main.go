package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vivolinux/internal/android"
	"vivolinux/internal/doctor"
	"vivolinux/internal/service"
)

const usage = `vivolinux: experimental native vivo LAN client and ADB/scrcpy fallback

Usage:
  vivolinux pair [--trust-scanned-phone | --phone-cert-sha256 HEX] [--phone LAN_IP] [--duration 5m]
                 [--persistent] [--clipboard] [--notifications] [--file-access] [--receive-dir DIRECTORY]
                 [--list-files PHONE_DIRECTORY] [--mirror-native]
  vivolinux native-status [--socket PATH]
  vivolinux files [--path PHONE_DIRECTORY | --category recent|images|videos|audio|documents] [--page 0] [--socket PATH]
  vivolinux download --remote PHONE_FILE --output NEW_LOCAL_FILE [--page 0]
  vivolinux upload --local LOCAL_FILE --to PHONE_DIRECTORY
  vivolinux thumbnail --remote PHONE_MEDIA [--width 128 --height 128]
  vivolinux stream --remote PHONE_MEDIA [--category images|videos --page 0]
  vivolinux preview --remote PHONE_MEDIA --output PRIVATE_CACHE_FILE
  vivolinux phone-certificate --phone LAN_IP
  vivolinux remote-pc [--state-dir PATH] [--desktop-helper PATH] [--country IN]
  vivolinux doctor
  vivolinux devices [--timeout 10s] [--via-service] [--socket PATH]
  vivolinux push --serial SERIAL [--timeout 2m] LOCAL ABSOLUTE_REMOTE
  vivolinux pull --serial SERIAL [--timeout 2m] ABSOLUTE_REMOTE LOCAL
  vivolinux mirror --serial SERIAL
  vivolinux camera --serial SERIAL
  vivolinux daemon [--socket PATH]
  vivolinux status [--socket PATH]

Flags must precede positional arguments. Doctor and device lists emit JSON.
The daemon stays in the foreground. The GTK desktop owns its paired background session.
ADB fallback commands require separately installed adb/scrcpy and authorized USB debugging.
Pair discovers the phone from a fresh QR scan; --phone is an optional address filter.
The QR bootstrap currently uses the India-region vivo rendezvous service.
Install qrencode; clipboard/notifications use make desktop's GTK helper. File commands
use the active native session and phone permission. The GTK frontend uses GStreamer for preview.
Push/pull explicitly write files and may overwrite existing files.
Rich/file clipboard remains planned; phone and connection notifications,
plain-text clipboard and virtual webcam output are implemented.
Mirror uses scrcpy's normal controls; camera displays a preview (Android 12+ required).
Remote PC shares this desktop through the phone's Office Kit Remote PC feature;
it requires screen-sharing permission and emits JSON lines. Input control depends on the desktop session.
Remote paths allow letters, digits, spaces, / . _ - and must not contain '..' segments.
This is not a complete vivo Office Kit port and cannot guarantee every phone or laptop.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], android.ExecRunner{}); err != nil {
		fmt.Fprintln(os.Stderr, "vivolinux:", err)
		os.Exit(1)
	}
}

func printJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func run(ctx context.Context, args []string, runner android.Runner) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(usage)
		return nil
	}
	command := args[0]
	if command == "native-status" || command == "files" || command == "download" || command == "upload" || command == "thumbnail" || command == "preview" || command == "stream" {
		return runNative(ctx, args)
	}
	if command == "pair" {
		return runPair(ctx, args[1:])
	}
	if command == "phone-certificate" {
		return runPhoneCertificate(ctx, args[1:])
	}
	if command == "remote-pc" {
		return runRemotePC(ctx, args[1:])
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	serial, socket, timeout, viaService := "", service.DefaultSocket(), 10*time.Second, false
	switch command {
	case "doctor":
	case "devices":
		flags.DurationVar(&timeout, "timeout", timeout, "ADB timeout")
		flags.BoolVar(&viaService, "via-service", false, "request devices from daemon")
		flags.StringVar(&socket, "socket", socket, "private Unix socket")
	case "push", "pull":
		timeout = 2 * time.Minute
		flags.StringVar(&serial, "serial", "", "explicit authorized ADB device serial")
		flags.DurationVar(&timeout, "timeout", timeout, "transfer timeout")
	case "mirror", "camera":
		flags.StringVar(&serial, "serial", "", "explicit authorized ADB device serial")
	case "daemon", "status":
		flags.StringVar(&socket, "socket", socket, "private Unix socket")
	default:
		return fmt.Errorf("unknown command %q; run vivolinux help", command)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	want := 0
	if command == "push" || command == "pull" {
		want = 2
	}
	if flags.NArg() != want {
		return fmt.Errorf("%s requires %d positional arguments; run vivolinux help", command, want)
	}
	if timeout <= 0 || timeout > 24*time.Hour {
		return errors.New("timeout must be greater than zero and at most 24h")
	}
	client := android.Client{Runner: runner}
	if command == "daemon" {
		return service.Serve(ctx, socket, client)
	}
	if command == "mirror" || command == "camera" {
		return client.Mirror(ctx, serial, command == "camera")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch command {
	case "doctor":
		return printJSON(doctor.Collect(ctx, runner))
	case "devices":
		if viaService {
			data, err := service.Call(ctx, socket, "devices")
			if err != nil {
				return err
			}
			return printJSON(data)
		}
		devices, err := client.Devices(ctx)
		if err != nil {
			return err
		}
		return printJSON(devices)
	case "push":
		return client.Transfer(ctx, "push", serial, flags.Arg(0), flags.Arg(1))
	case "pull":
		return client.Transfer(ctx, "pull", serial, flags.Arg(1), flags.Arg(0))
	case "status":
		data, err := service.Call(ctx, socket, "status")
		if err != nil {
			return err
		}
		return printJSON(data)
	}
	return nil
}
