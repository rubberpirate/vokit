package vivo

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

type MirrorInfo struct {
	MimeType  string `json:"mime_type"`
	SessionID int    `json:"sessionId"`
	Width     int    `json:"video_width"`
	Height    int    `json:"video_height"`
}
type MirrorOptions struct {
	Input        <-chan InputEvent
	DeviceInfo   func(MirrorInfo)
	MaxSize      int
	FrameRate    int
	EnableTiming bool
}

func MirrorInteractive(ctx context.Context, s *Session, address netip.Addr, firstUse bool, pin string, output io.Writer, options MirrorOptions, event func(Event)) error {
	if address.Zone() != "" || !address.Unmap().IsPrivate() {
		return errors.New("invalid media LAN address")
	}
	return mirrorVideoOptions(ctx, s, "https://"+net.JoinHostPort(address.Unmap().String(), "10381"), firstUse, pin, output, options, event)
}

// MirrorVideo is an experimental, permission-gated video-only native adapter.
// The caller must first receive res_authrity auth=1 on the main control channel.
func MirrorVideo(ctx context.Context, s *Session, address netip.Addr, firstUse bool, pin string, output io.Writer, event func(Event)) error {
	if address.Zone() != "" || !address.Unmap().IsPrivate() {
		return errors.New("invalid media LAN address")
	}
	return mirrorVideo(ctx, s, "https://"+net.JoinHostPort(address.Unmap().String(), "10381"), firstUse, pin, output, event)
}

func mirrorVideo(ctx context.Context, s *Session, base string, firstUse bool, pin string, output io.Writer, event func(Event)) error {
	return mirrorVideoOptions(ctx, s, base, firstUse, pin, output, MirrorOptions{}, event)
}
func mirrorVideoOptions(ctx context.Context, s *Session, base string, firstUse bool, pin string, output io.Writer, options MirrorOptions, event func(Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if s == nil || s.Token == "" || output == nil {
		return errors.New("missing media session or output")
	}
	var clients []*PhoneClient
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()
	dial := func(path string) (*websocket.Conn, error) {
		client, err := newPhoneClientTrust(base, pin, firstUse)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
		dialCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		header := http.Header{"Token": []string{s.Token}}
		conn, _, err := websocket.Dial(dialCtx, strings.Replace(base, "https://", "wss://", 1)+path, &websocket.DialOptions{HTTPClient: client.http, HTTPHeader: header, Subprotocols: []string{"v1.hc.vivo.com.cn"}})
		if err != nil {
			return nil, errors.New("native media WebSocket upgrade failed")
		}
		return conn, nil
	}
	video, err := dial("/mirror/screen")
	if err != nil {
		return err
	}
	defer video.CloseNow()
	video.SetReadLimit(MaxVideoFrameBytes)
	control, err := dial("/mirror/control")
	if err != nil {
		return err
	}
	defer control.CloseNow()
	control.SetReadLimit(maxResponseBytes)
	// No input/audio/drag is advertised until its own roundtrip is implemented.
	start := map[string]any{"app_package_name": "mirror", "device_type": 1, "first_file_open": true, "frame_with_time": true, "image_quality": 2, "max_size": 1280, "mime_type": "video/avc", "mime_type2": []string{"h264MP", "h264BP"}, "mirror_type": 0, "msg_send_key_mode": "0", "need_open_file": false, "no_audio": true, "pc_version": "0", "refresh_rate": 30, "screenPrivacy": false, "show_touch_spot": false, "split_frame": false, "support_drag": false, "video_height": 1560, "video_width": 720, "winId": "0"}
	payload, _ := json.Marshal(start)
	if options.MaxSize >= 640 && options.MaxSize <= 3840 {
		start["max_size"] = options.MaxSize
	}
	if options.FrameRate >= 1 && options.FrameRate <= 60 {
		start["refresh_rate"] = options.FrameRate
	}
	payload, _ = json.Marshal(start)
	writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	err = video.Write(writeCtx, websocket.MessageText, append([]byte("SCREEN_START:"), payload...))
	stop()
	if err != nil {
		return errors.New("native screen start failed")
	}
	emit := func(stage string, count int) {
		if event != nil {
			event(Event{Time: time.Now().UTC(), Stage: stage, Count: count})
		}
	}
	emit("mirror_stream_connected", 0)
	var sessionID atomic.Int64
	var haveInfo atomic.Bool
	var clock MediaClock
	var inputWorkers sync.WaitGroup
	if options.EnableTiming {
		inputWorkers.Add(1)
		go func() {
			defer inputWorkers.Done()
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				writeCtx, done := context.WithTimeout(ctx, time.Second)
				err := control.Write(writeCtx, websocket.MessageText, clock.Request(time.Now()))
				done()
				if err != nil {
					cancel()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	if options.Input != nil {
		inputWorkers.Add(1)
		go func() {
			defer inputWorkers.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case input, ok := <-options.Input:
					if !ok {
						return
					}
					if !haveInfo.Load() {
						continue
					}
					data, err := EncodeInput(input, int(sessionID.Load()))
					if err != nil {
						continue
					}
					writeCtx, done := context.WithTimeout(ctx, time.Second)
					err = control.Write(writeCtx, websocket.MessageText, data)
					done()
					if err != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	defer func() { cancel(); inputWorkers.Wait() }()
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		for {
			_, data, err := control.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			if options.EnableTiming {
				accepted := clock.Reply(data, time.Now())
				if strings.HasPrefix(string(data), "NTP:") && event != nil {
					// Report only known field types, never peer timestamps or values.
					var fields map[string]json.RawMessage
					valid := json.Unmarshal(data[4:], &fields) == nil
					shape := map[string]bool{"accepted": accepted, "json_object": valid && fields != nil, "three_fields": len(fields) == 3}
					for _, key := range []string{"t1", "t2", "t3"} {
						value := strings.TrimSpace(string(fields[key]))
						shape[key+"_present"] = value != ""
						shape[key+"_string"] = strings.HasPrefix(value, `"`)
					}
					event(Event{Time: time.Now().UTC(), Stage: "mirror_clock_reply", Capabilities: shape})
				}
			}
		}
	}()
	defer func() { cancel(); control.CloseNow(); <-controlDone }()
	frames := 0
	for {
		readCtx, done := context.WithTimeout(ctx, 15*time.Second)
		_, data, err := video.Read(readCtx)
		receivedAt := time.Now()
		done()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("native video disconnected or timed out")
		}
		if strings.HasPrefix(string(data), "DEVICE_INFO:") {
			var info MirrorInfo
			if json.Unmarshal(data[len("DEVICE_INFO:"):], &info) != nil || (info.MimeType != "" && info.MimeType != "video/avc") {
				return errors.New("phone selected an unsupported video codec")
			}
			if info.SessionID < 0 || info.Width < 0 || info.Width > 16384 || info.Height < 0 || info.Height > 16384 {
				return errors.New("invalid video geometry or session")
			}
			sessionID.Store(int64(info.SessionID))
			haveInfo.Store(true)
			if options.DeviceInfo != nil {
				options.DeviceInfo(info)
			}
			emit("mirror_device_info", 0)
			c, done := context.WithTimeout(ctx, 3*time.Second)
			command, _ := json.Marshal(map[string]int{"action": 6, "id": 0, "sessionId": info.SessionID})
			err := control.Write(c, websocket.MessageText, append([]byte("COMMAND_EVENT:"), command...))
			done()
			if err != nil {
				return errors.New("video keyframe request failed")
			}
			continue
		}
		_, annexB, err := ParseVideoFrame(data)
		if err != nil {
			continue
		} // Control/status packets are not video frames.
		var timing MediaTiming
		if options.EnableTiming {
			timing = clock.Sample(data[:16], receivedAt)
		}
		if deadline, ok := output.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = deadline.SetWriteDeadline(time.Now().Add(2 * time.Second))
		}
		if n, err := output.Write(annexB); err != nil || n != len(annexB) {
			return errors.New("native video output stopped or blocked")
		}
		frames++
		if frames == 1 || frames%30 == 0 {
			emit("mirror_video_frames", frames)
			if options.EnableTiming && event != nil {
				event(Event{Time: time.Now().UTC(), Stage: "mirror_frame_timing", Count: frames, MediaTiming: &timing})
			}
		}
	}
}
