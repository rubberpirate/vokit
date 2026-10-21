package vivo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

// CameraSnapshot keeps raw phone configuration private; callers should expose
// only selected capability fields in UI/status responses, never log the payload.
type CameraSnapshot struct {
	AuthorizationKnown bool
	Authorized         bool
	ServerEnabled      bool
	RunStateKnown      bool
	RunState           int
	Config             json.RawMessage `json:"-"`
	Capabilities       json.RawMessage `json:"-"`
}

type CameraState struct {
	mu    sync.Mutex
	state CameraSnapshot
}

func (s *CameraState) Snapshot() CameraSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := s.state
	result.Config = append(json.RawMessage(nil), result.Config...)
	result.Capabilities = append(json.RawMessage(nil), result.Capabilities...)
	return result
}

// ApplyMessage follows the vendor's shallow, non-null config/capability merge.
func (s *CameraState) ApplyMessage(data []byte) (bool, error) {
	command, raw, ok := bytes.Cut(data, []byte(":"))
	if !ok {
		return false, nil
	}
	switch string(command) {
	case "PC_RESPONSE_CAMERA_AUTH", "PC_GET_CAMERA_STATUS", "PHONE_TO_PC_CAMERA_RUN_STATE", "PC_GET_CAMERA_CONFIG", "PC_GET_CAMERA_CAPABILITY":
	default:
		return false, nil
	}
	if string(command) == "PHONE_TO_PC_CAMERA_RUN_STATE" && len(raw) <= maxResponseBytes {
		var nested string
		if json.Unmarshal(raw, &nested) == nil {
			raw = []byte(nested)
		}
	}
	fields, err := cameraObject(raw)
	if err != nil {
		return true, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch string(command) {
	case "PC_RESPONSE_CAMERA_AUTH":
		auth, err := cameraInteger(fields["auth"])
		if err != nil {
			return true, err
		}
		s.state.AuthorizationKnown, s.state.Authorized = true, auth == 1
	case "PC_GET_CAMERA_STATUS", "PHONE_TO_PC_CAMERA_RUN_STATE":
		value, err := cameraInteger(fields["state"])
		if err != nil {
			return true, err
		}
		if string(command) == "PC_GET_CAMERA_STATUS" {
			s.state.ServerEnabled = value == 1
		} else {
			s.state.RunStateKnown, s.state.RunState = true, value
		}
	case "PC_GET_CAMERA_CONFIG", "PC_GET_CAMERA_CAPABILITY":
		target := &s.state.Config
		if string(command) == "PC_GET_CAMERA_CAPABILITY" {
			target = &s.state.Capabilities
		}
		previous := make(map[string]json.RawMessage)
		if len(*target) != 0 {
			_ = json.Unmarshal(*target, &previous)
		}
		for key, value := range fields {
			if !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				previous[key] = value
			}
		}
		merged, err := json.Marshal(previous)
		if err != nil || len(merged) > maxResponseBytes {
			return true, errors.New("camera configuration exceeds size limit")
		}
		*target = merged
	}
	return true, nil
}

func cameraObject(data []byte) (map[string]json.RawMessage, error) {
	var result map[string]json.RawMessage
	if len(data) > maxResponseBytes || json.Unmarshal(data, &result) != nil || result == nil {
		return nil, errors.New("invalid or oversized camera configuration")
	}
	return result, nil
}

func cameraInteger(data []byte) (int, error) {
	var value int
	if len(data) > 20 || len(data) == 0 {
		return 0, errors.New("invalid camera state")
	}
	if json.Unmarshal(data, &value) == nil {
		return value, nil
	}
	var text string
	if json.Unmarshal(data, &text) == nil && json.Unmarshal([]byte(text), &value) == nil {
		return value, nil
	}
	return 0, errors.New("invalid camera state")
}

func CameraPermissionRequest(startFrom string, shareMode int) ([]byte, error) {
	if startFrom == "" || len(startFrom) > 200 || strings.ContainsFunc(startFrom, unicode.IsControl) || (shareMode != 1 && shareMode != 2) {
		return nil, errors.New("invalid camera permission request")
	}
	body, _ := json.Marshal(struct {
		Mode int    `json:"share_mode"`
		From string `json:"start_from"`
	}{shareMode, startFrom})
	return append([]byte("PC_REQUEST_CAMERA_AUTH:"), body...), nil
}

func CameraConfigRequest() []byte { return []byte("PC_GET_CAMERA_CONFIG:") }
func CameraEnableRequest() []byte { return []byte("PC_ENABLE_CAMERA_AUTH:") }

func CameraShareMode(config json.RawMessage) (int, error) {
	settings, err := cameraObject(config)
	if err != nil {
		return 0, err
	}
	mode, err := cameraInteger(settings["cameraShareMode"])
	if err != nil || (mode != 1 && mode != 2) {
		return 0, errors.New("camera sharing mode is not negotiated")
	}
	return mode, nil
}

// CameraSwitchCommand uses string selectors from the shipped camera UI and
// requires them in the phone's selected-mode cameraInfos capability list.
func CameraSwitchCommand(config, capabilities json.RawMessage, target string) ([]byte, error) {
	settings, err := cameraObject(config)
	if err != nil {
		return nil, err
	}
	caps, err := cameraObject(capabilities)
	if err != nil {
		return nil, err
	}
	mode, err := CameraShareMode(config)
	if err != nil {
		return nil, err
	}
	configKey, capabilityKey, rear := "normalCameraConfig", "normalCameraCapability", "Back"
	if mode == 2 {
		configKey, capabilityKey, rear = "liveCameraConfig", "liveCameraCapability", "Master"
	}
	if target == "front" {
		target = "frontMaster"
	} else if target == "rear" {
		target = rear
	}
	if target == "" || len(target) > 64 || strings.ContainsFunc(target, unicode.IsControl) {
		return nil, errors.New("invalid camera selector")
	}
	var available struct {
		Cameras []string `json:"cameraInfos"`
	}
	if json.Unmarshal(caps[capabilityKey], &available) != nil || len(available.Cameras) > 32 {
		return nil, errors.New("invalid camera capabilities")
	}
	advertised := false
	for _, value := range available.Cameras {
		advertised = advertised || value == target
	}
	if !advertised {
		return nil, errors.New("requested camera is not advertised by the phone")
	}
	selected, err := cameraObject(settings[configKey])
	if err != nil {
		return nil, err
	}
	selected["cameraType"], _ = json.Marshal(target)
	settings[configKey], _ = json.Marshal(selected)
	body, err := json.Marshal(settings)
	if err != nil || len(body) > maxResponseBytes-len("PC_UPDATE_CAMERA_PARA:") {
		return nil, errors.New("camera configuration exceeds size limit")
	}
	return append([]byte("PC_UPDATE_CAMERA_PARA:"), body...), nil
}

type CameraVideoOptions struct {
	FirstUse        bool
	Pin             string
	Authorized      bool
	EnableTiming    bool
	AudioProbe      bool
	Audio           func([]byte) error
	AudioCapability func(bool)
}

// CameraVideo writes Annex-B H.264 only, after auth=1 on main control WSS.
// Its writer must support write deadlines so a stalled decoder cannot block
// cancellation. Camera switching remains on main WSS, using CameraSwitchCommand.
func CameraVideo(ctx context.Context, s *Session, address netip.Addr, options CameraVideoOptions, output io.Writer, event func(Event)) error {
	if address.Zone() != "" || !address.Unmap().IsPrivate() {
		return errors.New("invalid camera LAN address")
	}
	return cameraVideo(ctx, s, "https://"+net.JoinHostPort(address.Unmap().String(), "10381"), options, output, event)
}

func cameraVideo(ctx context.Context, s *Session, base string, options CameraVideoOptions, output io.Writer, event func(Event)) error {
	deadlineOutput, ok := output.(interface{ SetWriteDeadline(time.Time) error })
	if s == nil || s.Token == "" || !options.Authorized || output == nil || !ok {
		return errors.New("camera requires explicit permission, a session, and a bounded output writer")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopOutput := context.AfterFunc(ctx, func() { _ = deadlineOutput.SetWriteDeadline(time.Now()) })
	defer stopOutput()
	var clients []*PhoneClient
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()
	dial := func(path string) (*websocket.Conn, error) {
		client, err := newPhoneClientTrust(base, options.Pin, options.FirstUse)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
		dialCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		conn, _, err := websocket.Dial(dialCtx, strings.Replace(base, "https://", "wss://", 1)+path, &websocket.DialOptions{HTTPClient: client.http, HTTPHeader: http.Header{"Token": []string{s.Token}}, Subprotocols: []string{"v1.hc.vivo.com.cn"}})
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("native camera WebSocket upgrade failed")
		}
		return conn, nil
	}
	video, err := dial("/camera_share/screen")
	if err != nil {
		return err
	}
	defer video.CloseNow()
	video.SetReadLimit(MaxVideoFrameBytes)
	control, err := dial("/camera_share/config")
	if err != nil {
		return err
	}
	defer control.CloseNow()
	control.SetReadLimit(maxResponseBytes)
	// Camera reference: image_quality=3, pc_version="650", requested 1x1.
	// Audio is opt-in; probe mode validates and discards complete AAC packets.
	start := map[string]any{"app_package_name": "mirror", "device_type": 1, "first_file_open": true, "frame_with_time": true, "image_quality": 3, "max_size": 1920, "mime_type": "video/avc", "mime_type2": []string{"h264MP", "h264BP"}, "mirror_type": 0, "msg_send_key_mode": "0", "need_open_file": false, "no_audio": !options.AudioProbe && options.Audio == nil, "pc_version": "650", "refresh_rate": 30, "screenPrivacy": false, "show_touch_spot": false, "split_frame": false, "support_drag": false, "video_height": 1, "video_width": 1, "winId": "0"}
	payload, _ := json.Marshal(start)
	writeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	err = video.Write(writeCtx, websocket.MessageText, append([]byte("SCREEN_START:"), payload...))
	stop()
	if err != nil {
		return errors.New("native camera start failed")
	}
	emit := func(stage string, count int) {
		if event != nil {
			event(Event{Time: time.Now().UTC(), Stage: stage, Count: count})
		}
	}
	emit("camera_stream_connected", 0)
	var clock MediaClock
	var timingWorker sync.WaitGroup
	if options.EnableTiming {
		// CameraSDK RVA 0x469d1 binds RequireDeltaTime (0x44620), which
		// sends Camera Performance.dll's NTP request on CLIENT_CONTROL.
		timingWorker.Add(1)
		go func() {
			defer timingWorker.Done()
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
	defer func() { cancel(); timingWorker.Wait() }()
	controlDone := make(chan struct{})
	go func() {
		defer close(controlDone)
		for {
			_, data, err := control.Read(ctx)
			if err != nil {
				cancel()
				return
			}
			if options.EnableTiming && bytes.HasPrefix(data, []byte("NTP:")) {
				accepted := clock.Reply(data, time.Now())
				if event != nil {
					event(Event{Time: time.Now().UTC(), Stage: "camera_clock_reply", Capabilities: map[string]bool{"accepted": accepted}})
				}
			}
		}
	}()
	defer func() { cancel(); control.CloseNow(); <-controlDone }()
	frames, audioPackets, negotiated := 0, 0, false
	for {
		readCtx, done := context.WithTimeout(ctx, 15*time.Second)
		_, data, err := video.Read(readCtx)
		done()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return nil
			}
			return errors.New("native camera disconnected or timed out")
		}
		if bytes.HasPrefix(data, []byte("DEVICE_INFO:")) {
			var info struct {
				MimeType  string          `json:"mime_type"`
				SessionID *int            `json:"sessionId"`
				Width     int             `json:"video_width"`
				Height    int             `json:"video_height"`
				WithAudio json.RawMessage `json:"with_audio"`
			}
			if len(data) > maxResponseBytes || json.Unmarshal(data[len("DEVICE_INFO:"):], &info) != nil || info.MimeType != "video/avc" || info.SessionID == nil || *info.SessionID < 0 || info.Width <= 0 || info.Height <= 0 || info.Width > 8192 || info.Height > 8192 {
				return errors.New("invalid or unsupported camera stream configuration")
			}
			negotiated = true
			emit("camera_device_info", 0)
			if options.AudioProbe || options.Audio != nil {
				var withAudio *bool
				if json.Unmarshal(info.WithAudio, &withAudio) == nil && withAudio != nil {
					if event != nil {
						event(Event{Time: time.Now().UTC(), Stage: "camera_audio_capability", Capabilities: map[string]bool{"with_audio": *withAudio}})
					}
					if options.AudioCapability != nil {
						options.AudioCapability(*withAudio)
					}
				}
			}
			command, _ := json.Marshal(map[string]int{"action": 6, "id": 0, "sessionId": *info.SessionID})
			writeCtx, done := context.WithTimeout(ctx, 3*time.Second)
			err := control.Write(writeCtx, websocket.MessageText, append([]byte("COMMAND_EVENT:"), command...))
			done()
			if err != nil {
				return errors.New("camera keyframe request failed")
			}
			continue
		}
		if (options.AudioProbe || options.Audio != nil) && negotiated && len(data) >= 23 && bytes.HasPrefix(data, []byte("FRAME:")) && cameraADTSFrames(data[16:]) > 0 {
			if options.Audio != nil {
				if err := options.Audio(data[16:]); err != nil {
					return errors.New("native camera audio output stopped or blocked")
				}
			}
			audioPackets++
			emit("camera_audio_packets", audioPackets)
			continue
		}
		header, annexB, err := ParseVideoFrame(data)
		if err != nil || !negotiated {
			continue
		}
		if options.EnableTiming && event != nil && (frames == 0 || frames%30 == 0) {
			// CameraSDK 0x40079 sign-extends encode time; 0x4009a–0x400c9
			// updates the same source/receiver clock telemetry as screen mode.
			timing := clock.Sample(header, time.Now())
			// Camera FRAME clock units differ on the tested phone; do not
			// report arrival timing until that clock domain is established.
			timing.ArrivalAgeMS, timing.ArrivalDriftMS, timing.ClockValid = nil, nil, false
			event(Event{Time: time.Now().UTC(), Stage: "camera_frame_timing", Count: frames + 1, MediaTiming: &timing})
		}
		if err := deadlineOutput.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return errors.New("camera output cannot enforce a write deadline")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if n, err := output.Write(annexB); err != nil || n != len(annexB) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("native camera output stopped or blocked")
		}
		frames++
		if frames == 1 || frames%30 == 0 {
			emit("camera_video_frames", frames)
		}
	}
}

// CameraSDK accepts AAC on CLIENT_VIDEO (RVA 0x41338). Count only whole ADTS
// sequences; this does not identify the audio source as the physical microphone.
func cameraADTSFrames(data []byte) int {
	if len(data) > MaxVideoFrameBytes-16 {
		return 0
	}
	frames := 0
	for len(data) > 0 {
		if len(data) < 7 || data[0] != 0xff || data[1]&0xf6 != 0xf0 || (data[2]>>2)&15 >= 13 {
			return 0
		}
		header := 7
		if data[1]&1 == 0 {
			header = 9
		}
		size := int(data[3]&3)<<11 | int(data[4])<<3 | int(data[5]>>5)
		if size <= header || size > len(data) {
			return 0
		}
		frames++
		data = data[size:]
	}
	return frames
}
