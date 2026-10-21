package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const nativeRequestLimit = 16 * 1024
const nativeResponseLimit = 1024 * 1024
const nativeCallTimeout = 10 * time.Minute

var ErrNativeFilePermission = errors.New("phone file permission is required; approve the Office Kit file prompt and retry")

type NativeRequest struct {
	Method    string `json:"method"`
	Category  string `json:"category,omitempty"`
	Directory string `json:"directory,omitempty"`
	Remote    string `json:"remote,omitempty"`
	Output    string `json:"output,omitempty"`
	Local     string `json:"local,omitempty"`
	Page      int    `json:"page,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
}

func DefaultNativeSocket() string {
	return filepath.Join(filepath.Dir(DefaultSocket()), "native.sock")
}

func nativeAbsolutePath(value string) bool {
	return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsAny(value, "\x00\\")
}

func ValidFileCategory(category string) bool {
	switch category {
	case "recent", "images", "videos", "audio", "documents":
		return true
	}
	return false
}

func validNativeRequest(req NativeRequest) bool {
	if req.Category != "" && (!ValidFileCategory(req.Category) || (req.Method != "files" && req.Method != "download" && req.Method != "thumbnail" && req.Method != "preview")) {
		return false
	}
	if req.Page < 0 {
		return false
	}
	if req.Method != "thumbnail" && (req.Width != 0 || req.Height != 0) {
		return false
	}
	switch req.Method {
	case "status":
		return req.Directory == "" && req.Remote == "" && req.Output == "" && req.Local == "" && req.Page == 0
	case "files":
		return ((req.Category == "" && nativeAbsolutePath(req.Directory)) || (req.Category != "" && req.Directory == "")) && req.Remote == "" && req.Output == "" && req.Local == ""
	case "download", "preview":
		return req.Directory == "" && nativeAbsolutePath(req.Remote) && nativeAbsolutePath(req.Output) && req.Local == ""
	case "thumbnail":
		return req.Directory == "" && nativeAbsolutePath(req.Remote) && req.Output == "" && req.Local == "" && req.Width > 0 && req.Width <= 1024 && req.Height > 0 && req.Height <= 1024
	case "upload":
		return nativeAbsolutePath(req.Local) && nativeAbsolutePath(req.Directory) && req.Remote == "" && req.Output == "" && req.Page == 0
	}
	return false
}

// ServeNative exposes the current in-memory phone session only inside a private
// user-owned directory. The handler must honor cancellation and enforce consent.
func ServeNative(ctx context.Context, socket string, handler func(context.Context, NativeRequest) (any, error)) error {
	return ServeNativeReady(ctx, socket, handler, nil)
}

// ServeNativeReady reports binding success before accepting local requests.
func ServeNativeReady(ctx context.Context, socket string, handler func(context.Context, NativeRequest) (any, error), ready func(error)) error {
	if handler == nil {
		err := errors.New("native request handler is required")
		if ready != nil {
			ready(err)
		}
		return err
	}
	listener, cleanup, err := listenPrivate(socket)
	if ready != nil {
		ready(err)
	}
	if err != nil {
		return err
	}
	defer cleanup()

	var mu sync.Mutex
	connections := make(map[*net.UnixConn]struct{})
	var workers sync.WaitGroup
	closeConnections := func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for conn := range connections {
			conn.Close()
		}
	}
	stop := context.AfterFunc(ctx, closeConnections)
	defer func() {
		stop()
		closeConnections()
		workers.Wait()
	}()
	slots := make(chan struct{}, 16)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("native socket accept failed")
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		mu.Lock()
		connections[conn] = struct{}{}
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				conn.Close()
				mu.Lock()
				delete(connections, conn)
				mu.Unlock()
				<-slots
			}()
			serveNativeConnection(ctx, conn, handler)
		}()
	}
}

func serveNativeConnection(ctx context.Context, conn *net.UnixConn, handler func(context.Context, NativeRequest) (any, error)) {
	requestCtx, cancel := context.WithTimeout(ctx, nativeCallTimeout)
	defer cancel()
	stop := context.AfterFunc(requestCtx, func() { conn.Close() })
	defer stop()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, nativeRequestLimit+1)
	data, err := reader.ReadSlice('\n')
	var req NativeRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err != nil || len(data) > nativeRequestLimit || decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || !validNativeRequest(req) {
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		json.NewEncoder(conn).Encode(Response{Error: "invalid native request"})
		return
	}
	deadline, _ := requestCtx.Deadline()
	conn.SetDeadline(deadline)
	// No second request is accepted; EOF or additional bytes cancel active work.
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		var one [1]byte
		reader.Read(one[:])
		cancel()
	}()
	defer func() { conn.Close(); <-watchDone }()
	result, err := handler(requestCtx, req)
	response := Response{Result: result}
	if err != nil {
		response = Response{Error: "native request failed"}
		if errors.Is(err, ErrNativeFilePermission) {
			response.Error = "file_permission_required"
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded)+1 > nativeResponseLimit {
		encoded = []byte(`{"error":"invalid native response"}`)
	}
	conn.Write(append(encoded, '\n'))
}

func CallNative(ctx context.Context, socket string, req NativeRequest) (json.RawMessage, error) {
	if !validNativeRequest(req) {
		return nil, errors.New("invalid native request")
	}
	data, err := json.Marshal(req)
	if err != nil || len(data)+1 > nativeRequestLimit {
		return nil, errors.New("native request exceeds size limit")
	}
	if err := privateDir(socket); err != nil {
		return nil, errors.New("native socket directory is not private")
	}
	info, err := os.Lstat(socket)
	if err != nil {
		return nil, errors.New("native session is unavailable; start pairing first")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("native socket is not a private user-owned socket")
	}
	ctx, cancel := context.WithTimeout(ctx, nativeCallTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot connect to native session")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	conn.SetDeadline(deadline)
	if _, err := conn.Write(append(data, '\n')); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot send native request")
	}
	data, err = bufio.NewReaderSize(conn, nativeResponseLimit+1).ReadSlice('\n')
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || len(data) > nativeResponseLimit {
		return nil, errors.New("invalid native response")
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if json.Unmarshal(data, &response) != nil {
		return nil, errors.New("invalid native response")
	}
	if response.Error != "" {
		if response.Error == "file_permission_required" {
			return nil, ErrNativeFilePermission
		}
		return nil, errors.New("native request failed")
	}
	if response.Result == nil {
		return nil, errors.New("native response has no result")
	}
	return response.Result, nil
}
