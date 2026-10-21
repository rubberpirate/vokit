// Package android provides an explicit ADB fallback, not vivo's proprietary protocol.
package android

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
	Stream(context.Context, string, ...string) error
}

type ExecRunner struct{}

// Limit diagnostic output from external programs while continuing to drain pipes.
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if left := 1024*1024 - b.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var output cappedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.Bytes(), err
}

func (ExecRunner) Stream(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

type Device struct {
	Serial     string            `json:"serial"`
	State      string            `json:"state"`
	Properties map[string]string `json:"properties,omitempty"`
}

func ParseDevices(output string) ([]Device, error) {
	devices := make([]Device, 0)
	foundHeader := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "List of devices attached" {
			foundHeader = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "* daemon") {
			continue
		}
		if !foundHeader {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("malformed adb device line: %q", line)
		}
		d := Device{Serial: fields[0], State: fields[1], Properties: make(map[string]string)}
		if strings.HasPrefix(strings.Join(fields[1:], " "), "no permissions") {
			d.State = "no permissions"
		}
		for _, field := range fields[2:] {
			if k, v, ok := strings.Cut(field, ":"); ok {
				d.Properties[k] = v
			}
		}
		devices = append(devices, d)
	}
	if !foundHeader {
		return nil, errors.New("adb response did not contain device list header")
	}
	return devices, nil
}

type Client struct{ Runner Runner }

func (c Client) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := c.Runner.Run(ctx, "adb", args...)
	if err != nil {
		return nil, fmt.Errorf("adb: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (c Client) Devices(ctx context.Context) ([]Device, error) {
	out, err := c.run(ctx, "devices", "-l")
	if err != nil {
		return nil, err
	}
	return ParseDevices(string(out))
}

func ValidateSerial(serial string) error {
	if serial == "" || len(serial) > 255 || strings.HasPrefix(serial, "-") {
		return errors.New("an explicit valid --serial is required")
	}
	for _, r := range serial {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-:[]", r)) {
			return errors.New("serial contains unsupported characters")
		}
	}
	return nil
}

func ValidateRemote(path string) error {
	if !strings.HasPrefix(path, "/") || len(path) > 4096 {
		return errors.New("remote path must be an absolute Android path")
	}
	for _, r := range path {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/._- ", r)) {
			return errors.New("remote path contains unsupported characters; use letters, digits, spaces, / . _ -")
		}
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return errors.New("remote path must not contain ..")
		}
	}
	return nil
}

func localPath(path string) (string, error) {
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("a nonempty local path without control characters is required")
	}
	return filepath.Abs(path)
}

func (c Client) Transfer(ctx context.Context, direction, serial, local, remote string) error {
	if direction != "push" && direction != "pull" {
		return errors.New("transfer must be push or pull")
	}
	if err := ValidateSerial(serial); err != nil {
		return err
	}
	if err := ValidateRemote(remote); err != nil {
		return err
	}
	abs, err := localPath(local)
	if err != nil {
		return err
	}
	if direction == "push" {
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("local source: %w", err)
		}
	}
	args := []string{"-s", serial, direction}
	if direction == "push" {
		args = append(args, abs, remote)
	} else {
		args = append(args, remote, abs)
	}
	_, err = c.run(ctx, args...)
	return err
}

func (c Client) Mirror(ctx context.Context, serial string, camera bool) error {
	if err := ValidateSerial(serial); err != nil {
		return err
	}
	args := []string{"--serial", serial}
	if camera {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := c.Runner.Run(probeCtx, "scrcpy", "--help")
		cancel()
		if err != nil {
			return fmt.Errorf("probe scrcpy: %w", err)
		}
		if !strings.Contains(string(out), "--video-source") || !strings.Contains(string(out), "--no-audio") {
			return errors.New("installed scrcpy does not expose camera video-source support; upgrade scrcpy (phone must support Android 12+ camera capture)")
		}
		args = append(args, "--video-source=camera", "--no-audio")
	}
	if err := c.Runner.Stream(ctx, "scrcpy", args...); err != nil {
		return fmt.Errorf("scrcpy: %w", err)
	}
	return nil
}

// Ensure the bounded writer obeys io.Writer's contract.
var _ io.Writer = (*cappedBuffer)(nil)
