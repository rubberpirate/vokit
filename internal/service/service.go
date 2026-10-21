// Package service exposes only local status and device enumeration, never phone writes.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"vivolinux/internal/android"
)

type Request struct {
	Method string `json:"method"`
}
type Response struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func DefaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "vivolinux", "service.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("vivolinux-%d", os.Getuid()), "service.sock")
}

func privateDir(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("socket path must be absolute")
	}
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("socket directory must be owned by this user, private (0700), and not a symlink")
	}
	return nil
}

func Serve(ctx context.Context, socket string, client android.Client) error {
	listener, cleanup, err := listenPrivate(socket)
	if err != nil {
		return err
	}
	defer cleanup()

	started := time.Now()
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-finished:
		}
	}()
	var handlers sync.WaitGroup
	defer handlers.Wait()
	slots := make(chan struct{}, 16)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		handlers.Add(1)
		go func() {
			defer handlers.Done()
			defer func() { <-slots }()
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			var req Request
			decoder := json.NewDecoder(io.LimitReader(conn, 4096))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&req); err != nil {
				_ = json.NewEncoder(conn).Encode(Response{Error: "invalid request"})
				return
			}
			response := Response{}
			switch req.Method {
			case "status":
				response.Result = map[string]any{"backend": "adb-fallback", "started_at": started.UTC(), "pid": os.Getpid(), "proprietary_vivo_protocol": false}
			case "devices":
				requestCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
				defer cancel()
				devices, err := client.Devices(requestCtx)
				if err != nil {
					response.Error = err.Error()
				} else {
					response.Result = devices
				}
			default:
				response.Error = "unsupported method; use status or devices"
			}
			_ = json.NewEncoder(conn).Encode(response)
		}()
	}
}

func Call(ctx context.Context, socket, method string) (json.RawMessage, error) {
	if method != "status" && method != "devices" {
		return nil, errors.New("unsupported RPC method")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(6 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(Request{Method: method}); err != nil {
		return nil, err
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(conn, 1024*1024)).Decode(&response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return response.Result, nil
}
