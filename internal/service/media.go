package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const MediaInfo byte = 1
const MediaVideo byte = 2
const MediaStatus byte = 3
const MediaAudio byte = 4
const MaxMediaPacket = 16 * 1024 * 1024

type MediaRequest struct {
	Source     string `json:"source"`
	Facing     string `json:"facing,omitempty"`
	Quality    string `json:"quality,omitempty"`
	AudioProbe bool   `json:"audio_probe,omitempty"`
	Microphone bool   `json:"microphone,omitempty"`
}

func validMediaRequest(r MediaRequest) bool {
	if r.Source == "screen" {
		return r.Facing == "" && r.Quality == "" && !r.AudioProbe && !r.Microphone
	}
	return r.Source == "camera" && (r.Facing == "front" || r.Facing == "rear") && (r.Quality == "" || r.Quality == "720p" || r.Quality == "1080p")
}

type MediaChannel struct {
	conn  *net.UnixConn
	mu    sync.Mutex
	Input <-chan json.RawMessage
}

func DefaultMediaSocket() string {
	return filepath.Join(filepath.Dir(DefaultNativeSocket()), "media.sock")
}
func (s *MediaChannel) SetWriteDeadline(t time.Time) error { return s.conn.SetWriteDeadline(t) }
func (s *MediaChannel) Packet(kind byte, payload []byte) error {
	if len(payload) > MaxMediaPacket {
		return errors.New("media packet exceeds limit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	header := [5]byte{kind}
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	buffers := net.Buffers{header[:], payload}
	_, err := buffers.WriteTo(s.conn)
	return err
}
func (s *MediaChannel) Write(payload []byte) (int, error) {
	if err := s.Packet(MediaVideo, payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}
func (s *MediaChannel) Info(info any) error {
	data, err := json.Marshal(info)
	if err != nil {
		return errors.New("cannot encode media metadata")
	}
	return s.Packet(MediaInfo, data)
}

// ServeMedia permits one camera and one screen consumer. Tokens stay private.
func ServeMedia(ctx context.Context, socket string, handler func(context.Context, MediaRequest, *MediaChannel) error) error {
	if handler == nil || privateDir(socket) != nil {
		return errors.New("media socket requires private state and a handler")
	}
	if _, err := os.Lstat(socket); !errors.Is(err, os.ErrNotExist) {
		return errors.New("media socket already exists or is unavailable")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return errors.New("cannot listen on media socket")
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return errors.New("cannot secure media socket")
	}
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	var sourceMu sync.Mutex
	sources := make(map[string]bool)
	acquire := func(source string) bool {
		sourceMu.Lock()
		defer sourceMu.Unlock()
		if sources[source] {
			return false
		}
		sources[source] = true
		return true
	}
	release := func(source string) { sourceMu.Lock(); delete(sources, source); sourceMu.Unlock() }
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer func() { stop(); listener.Close(); workers.Wait() }()
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return errors.New("media socket accept failed")
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = (&MediaChannel{conn: conn}).Packet(MediaStatus, []byte(`{"error":"Another preview is already open."}`))
			conn.Close()
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots }()
			defer conn.Close()
			serveMediaConnection(ctx, conn, func(ctx context.Context, request MediaRequest, channel *MediaChannel) error {
				if !acquire(request.Source) {
					_ = channel.Packet(MediaStatus, []byte(`{"error":"This phone source is already open."}`))
					return nil
				}
				defer release(request.Source)
				return handler(ctx, request, channel)
			})
		}()
	}
}
func serveMediaConnection(parent context.Context, conn *net.UnixConn, handler func(context.Context, MediaRequest, *MediaChannel) error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, 16*1024+1)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 1024 {
		return
	}
	var request MediaRequest
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || !validMediaRequest(request) {
		return
	}
	conn.SetReadDeadline(time.Time{})
	commands := make(chan json.RawMessage, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		for {
			data, err := reader.ReadSlice('\n')
			if err != nil || len(data) > 16*1024 || !json.Valid(data) {
				return
			}
			copyData := append(json.RawMessage(nil), data...)
			select {
			case commands <- copyData:
			case <-ctx.Done():
				return
			default:
				return
			}
		}
	}()
	channel := &MediaChannel{conn: conn, Input: commands}
	defer func() { cancel(); conn.Close(); <-done }()
	if err := handler(ctx, request, channel); err != nil && ctx.Err() == nil {
		_ = channel.Packet(MediaStatus, []byte(`{"error":"Media stopped. Check phone permission and connection."}`))
	}
}
