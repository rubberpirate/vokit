package vivo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCameraStateRequiresExplicitAuthorizationAndMergesBoundedConfiguration(t *testing.T) {
	var state CameraState
	if state.Snapshot().Authorized {
		t.Fatal("camera began authorized")
	}
	for _, data := range []string{
		`PC_GET_CAMERA_CONFIG:{"cameraShareMode":1,"version":4,"normalCameraConfig":{"cameraType":"Back","frameRate":30},"privateField":"retained"}`,
		`PC_GET_CAMERA_CONFIG:{"version":null,"timestamp":123}`,
		`PC_GET_CAMERA_CAPABILITY:{"normalCameraCapability":{"cameraInfos":["frontMaster","Back"]}}`,
		`PC_GET_CAMERA_STATUS:{"state":1}`,
		`PHONE_TO_PC_CAMERA_RUN_STATE:{"state":0}`,
		`PC_RESPONSE_CAMERA_AUTH:{"auth":"1","start_from":"fixture"}`,
	} {
		if handled, err := state.ApplyMessage([]byte(data)); !handled || err != nil {
			t.Fatal("camera message not accepted", err)
		}
	}
	got := state.Snapshot()
	if !got.AuthorizationKnown || !got.Authorized || !got.ServerEnabled || got.RunState != 0 || !bytes.Contains(got.Config, []byte(`"version":4`)) {
		t.Fatal("camera state lost authorization or configuration")
	}
	got.Config[0] = '!'
	if !json.Valid(state.Snapshot().Config) {
		t.Fatal("snapshot mutated stored configuration")
	}
	for _, auth := range []string{`0`, `2`, `"2"`} {
		if _, err := state.ApplyMessage([]byte(`PC_RESPONSE_CAMERA_AUTH:{"auth":` + auth + `}`)); err != nil || state.Snapshot().Authorized {
			t.Fatal("camera denial granted access", err)
		}
	}
	if handled, err := state.ApplyMessage([]byte("CLIPBOARD_TEXT:private")); handled || err != nil {
		t.Fatal("unrelated control message interpreted as camera data")
	}
	for _, raw := range []string{`PC_GET_CAMERA_CONFIG:null`, `PC_RESPONSE_CAMERA_AUTH:{}`, `PC_RESPONSE_CAMERA_AUTH:{"auth":true}`, "PC_GET_CAMERA_CONFIG:" + strings.Repeat("x", maxResponseBytes)} {
		if _, err := state.ApplyMessage([]byte(raw)); err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid camera message accepted or leaked", err)
		}
	}
}

func TestCameraSwitchUsesAdvertisedStringSelectorsAndPreservesConfiguration(t *testing.T) {
	caps := json.RawMessage(`{"normalCameraCapability":{"cameraInfos":["frontMaster","Back"]},"liveCameraCapability":{"cameraInfos":["frontMaster","Master","Wide"]}}`)
	for _, test := range []struct {
		mode         int
		target, want string
	}{
		{1, "front", "frontMaster"}, {1, "rear", "Back"}, {2, "rear", "Master"}, {2, "Wide", "Wide"},
	} {
		config, _ := json.Marshal(map[string]any{"cameraShareMode": test.mode, "version": 7, "normalCameraConfig": map[string]any{"cameraType": "Back", "frameRate": 30}, "liveCameraConfig": map[string]any{"cameraType": "Master", "frameRate": 60}})
		command, err := CameraSwitchCommand(config, caps, test.target)
		if err != nil || !bytes.HasPrefix(command, []byte("PC_UPDATE_CAMERA_PARA:")) {
			t.Fatal("camera switch command missing", err)
		}
		var body struct {
			Version int
			Normal  struct {
				CameraType string
				FrameRate  int
			} `json:"normalCameraConfig"`
			Live struct {
				CameraType string
				FrameRate  int
			} `json:"liveCameraConfig"`
		}
		if json.Unmarshal(command[len("PC_UPDATE_CAMERA_PARA:"):], &body) != nil || body.Version != 7 || body.Normal.FrameRate != 30 || body.Live.FrameRate != 60 {
			t.Fatal("camera switch lost unrelated settings")
		}
		selected := body.Normal.CameraType
		if test.mode == 2 {
			selected = body.Live.CameraType
		}
		if selected != test.want {
			t.Fatal("wrong camera selector", selected)
		}
	}
	for _, target := range []string{"1", "rear", "Tele3P5xExternal"} {
		if _, err := CameraSwitchCommand(json.RawMessage(`{"cameraShareMode":1,"normalCameraConfig":{"cameraType":"frontMaster"}}`), json.RawMessage(`{"normalCameraCapability":{"cameraInfos":["frontMaster"]}}`), target); err == nil {
			t.Fatal("unadvertised camera accepted")
		}
	}
	command, err := CameraPermissionRequest("Linux fixture", 1)
	if err != nil || string(command) != `PC_REQUEST_CAMERA_AUTH:{"share_mode":1,"start_from":"Linux fixture"}` || string(CameraConfigRequest()) != "PC_GET_CAMERA_CONFIG:" || string(CameraEnableRequest()) != "PC_ENABLE_CAMERA_AUTH:" {
		t.Fatal("incorrect main camera control commands", err)
	}
	if _, err := CameraPermissionRequest("fixture", 3); err == nil {
		t.Fatal("unproven camera mode accepted")
	}
}

func TestCameraShareModeRequiresNegotiatedNormalOrLiveMode(t *testing.T) {
	for _, mode := range []int{1, 2} {
		config, _ := json.Marshal(map[string]int{"cameraShareMode": mode})
		if got, err := CameraShareMode(config); err != nil || got != mode {
			t.Fatal("valid camera sharing mode rejected", err)
		}
	}
	for _, config := range []string{`null`, `{}`, `{"cameraShareMode":0}`, `{"cameraShareMode":3}`, `{"cameraShareMode":true}`, `{"cameraShareMode":1.5}`} {
		if _, err := CameraShareMode(json.RawMessage(config)); err == nil {
			t.Fatal("missing or unsupported camera sharing mode accepted")
		}
	}
}

func TestCameraStateBoundsAccumulatedConfiguration(t *testing.T) {
	var state CameraState
	first := []byte(`PC_GET_CAMERA_CONFIG:{"first":"` + strings.Repeat("x", 300*1024) + `"}`)
	if _, err := state.ApplyMessage(first); err != nil {
		t.Fatal(err)
	}
	before := state.Snapshot().Config
	if _, err := state.ApplyMessage([]byte(`PC_GET_CAMERA_CONFIG:{"second":"` + strings.Repeat("x", 300*1024) + `"}`)); err == nil {
		t.Fatal("accumulated camera configuration exceeded bound")
	}
	if !bytes.Equal(before, state.Snapshot().Config) {
		t.Fatal("oversized update partially changed camera state")
	}
}

func TestCameraRunStateAcceptsSourceNestedJSON(t *testing.T) {
	var state CameraState
	if _, err := state.ApplyMessage([]byte(`PHONE_TO_PC_CAMERA_RUN_STATE:"{\"state\":1}"`)); err != nil {
		t.Fatal("nested camera run-state response rejected", err)
	}
	if snapshot := state.Snapshot(); !snapshot.RunStateKnown || snapshot.RunState != 1 {
		t.Fatal("nested camera run state was not applied")
	}
}

type cameraOutput struct {
	sync.Mutex
	data bytes.Buffer
}

func (w *cameraOutput) SetWriteDeadline(time.Time) error { return nil }
func (w *cameraOutput) Write(data []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	return w.data.Write(data)
}

func TestCameraVideoUsesCameraSocketsAndNegotiatedSessionForKeyframe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	keyframe := make(chan struct{})
	annexB := []byte{0, 0, 0, 1, 0x67, 0x42}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Token") != "fixture-token" || r.Header.Get("Sec-WebSocket-Protocol") != "v1.hc.vivo.com.cn" {
			t.Error("camera authentication contract")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v1.hc.vivo.com.cn"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		switch r.URL.Path {
		case "/camera_share/config":
			_, data, err := conn.Read(ctx)
			if err != nil || string(data) != `COMMAND_EVENT:{"action":6,"id":0,"sessionId":17}` {
				t.Error("camera keyframe command", err)
			}
			close(keyframe)
			_, _, _ = conn.Read(ctx)
		case "/camera_share/screen":
			_, data, err := conn.Read(ctx)
			if err != nil || !bytes.HasPrefix(data, []byte("SCREEN_START:")) {
				t.Error("camera start missing")
				return
			}
			var settings map[string]any
			_ = json.Unmarshal(data[len("SCREEN_START:"):], &settings)
			if settings["no_audio"] != true || settings["support_drag"] != false || settings["image_quality"] != float64(3) || settings["video_width"] != float64(1) || settings["pc_version"] != "650" {
				t.Error("camera preset differs from recovered video-only contract")
			}
			_ = conn.Write(ctx, websocket.MessageText, []byte(`DEVICE_INFO:{"mime_type":"video/avc","sessionId":17,"video_width":1920,"video_height":1080}`))
			select {
			case <-keyframe:
			case <-ctx.Done():
				return
			}
			_ = conn.Write(ctx, websocket.MessageBinary, append([]byte("FRAME:\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), annexB...))
			_ = conn.Close(websocket.StatusNormalClosure, "")
		default:
			t.Error("wrong camera socket")
		}
	}))
	defer server.Close()
	output := &cameraOutput{}
	err := cameraVideo(ctx, &Session{Token: "fixture-token"}, server.URL, CameraVideoOptions{FirstUse: true, Authorized: true}, output, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.data.Bytes(), annexB) {
		t.Fatal("camera frame envelope was not stripped")
	}
}

func TestCameraAudioProbeCountsOnlyCompleteADTSAndKeepsVideoOutput(t *testing.T) {
	for _, name := range []string{"default", "probe", "microphone"} {
		probe := name != "default"
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			keyframe := make(chan struct{})
			annexB := []byte{0, 0, 0, 1, 0x67, 0x42}
			aac := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0}
			frame := func(payload []byte) []byte {
				return append([]byte("FRAME:\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), payload...)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v1.hc.vivo.com.cn"}})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				switch r.URL.Path {
				case "/camera_share/config":
					_, _, _ = conn.Read(ctx)
					close(keyframe)
					_, _, _ = conn.Read(ctx)
				case "/camera_share/screen":
					_, data, err := conn.Read(ctx)
					var settings struct {
						NoAudio *bool `json:"no_audio"`
					}
					if err != nil || !bytes.HasPrefix(data, []byte("SCREEN_START:")) || json.Unmarshal(data[13:], &settings) != nil || settings.NoAudio == nil || *settings.NoAudio == probe {
						t.Error("audio probe was not explicit")
						return
					}
					_ = conn.Write(ctx, websocket.MessageBinary, frame(aac))
					_ = conn.Write(ctx, websocket.MessageText, []byte(`DEVICE_INFO:{"mime_type":"video/avc","sessionId":17,"video_width":1280,"video_height":720,"with_audio":true,"device_model":"private ignored field"}`))
					select {
					case <-keyframe:
					case <-ctx.Done():
						return
					}
					for _, payload := range [][]byte{aac, append(append([]byte{}, aac...), aac...), aac[:7], annexB} {
						if conn.Write(ctx, websocket.MessageBinary, frame(payload)) != nil {
							return
						}
					}
					_ = conn.Close(websocket.StatusNormalClosure, "")
				default:
					t.Error("unexpected camera endpoint")
				}
			}))
			defer server.Close()
			output := &cameraOutput{}
			var events []Event
			options := CameraVideoOptions{FirstUse: true, Authorized: true, AudioProbe: name == "probe"}
			audioCalls, capabilityCalls := 0, 0
			if name == "microphone" {
				options.Audio = func(data []byte) error {
					audioCalls++
					if cameraADTSFrames(data) == 0 {
						t.Error("invalid audio forwarded")
					}
					return nil
				}
				options.AudioCapability = func(available bool) {
					capabilityCalls++
					if !available {
						t.Error("capability changed")
					}
				}
			}
			err := cameraVideo(ctx, &Session{Token: "fixture-token"}, server.URL, options, output, func(event Event) { events = append(events, event) })
			if name == "microphone" && (audioCalls != 2 || capabilityCalls != 1) {
				t.Fatal("audio callback ignored negotiation or validation")
			}
			if err != nil || !bytes.Equal(output.data.Bytes(), annexB) {
				t.Fatal("audio altered camera output or shutdown")
			}
			capabilities, packets, last := 0, 0, 0
			for _, event := range events {
				if event.Stage == "camera_audio_capability" {
					capabilities++
					if !event.Capabilities["with_audio"] {
						t.Fatal("audio capability lost")
					}
				}
				if event.Stage == "camera_audio_packets" {
					packets++
					last = event.Count
				}
			}
			if probe {
				if capabilities != 1 || packets != 2 || last != 2 {
					t.Fatal("audio counts include unnegotiated or partial data")
				}
			} else if capabilities != 0 || packets != 0 {
				t.Fatal("default enabled audio probe")
			}
			data, _ := json.Marshal(events)
			for _, secret := range []string{"fixture-token", "private ignored field", "sessionId", "FRAME:"} {
				if bytes.Contains(data, []byte(secret)) {
					t.Fatal("audio event leaked private fields")
				}
			}
		})
	}
}

func TestCameraADTSFramesRejectsPartialAndOversizedPayloads(t *testing.T) {
	frame := []byte{0xff, 0xf1, 0x50, 0x80, 0x01, 0x1f, 0xfc, 0}
	if cameraADTSFrames(frame) != 1 || cameraADTSFrames(append(append([]byte{}, frame...), frame...)) != 2 {
		t.Fatal("complete ADTS not recognized")
	}
	for _, data := range [][]byte{nil, frame[:7], append(append([]byte{}, frame...), 0), {0, 0, 0, 1, 0x67}, bytes.Repeat(frame, MaxVideoFrameBytes/len(frame))} {
		if cameraADTSFrames(data) != 0 {
			t.Fatal("invalid ADTS accepted")
		}
	}
}

func TestCameraVideoTimingCalibratesOnConfigSocketAndReportsDurationsOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	calibrated := make(chan struct{})
	periodic := make(chan struct{})
	annexB := []byte{0, 0, 0, 1, 0x67, 0x42}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v1.hc.vivo.com.cn"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		switch r.URL.Path {
		case "/camera_share/config":
			requests := 0
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				if bytes.HasPrefix(data, []byte("COMMAND_EVENT:")) {
					continue
				}
				if !bytes.HasPrefix(data, []byte("NTP:")) {
					t.Error("unexpected camera timing command")
					return
				}
				var request struct {
					T1 int64 `json:"t1"`
				}
				if json.Unmarshal(data[4:], &request) != nil || request.T1 <= 0 {
					t.Error("invalid camera NTP request")
					return
				}
				now := time.Now().UnixMilli()
				reply, _ := json.Marshal(map[string]any{"t1": request.T1, "t2": now, "t3": now, "extra": "private ignored field"})
				if conn.Write(ctx, websocket.MessageText, append([]byte("NTP:"), reply...)) != nil {
					return
				}
				requests++
				if requests == 2 {
					close(periodic)
				}
			}
		case "/camera_share/screen":
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
			_ = conn.Write(ctx, websocket.MessageText, []byte(`DEVICE_INFO:{"mime_type":"video/avc","sessionId":17,"video_width":1920,"video_height":1080}`))
			select {
			case <-calibrated:
			case <-ctx.Done():
				return
			}
			frame := append(timingHeader(time.Now().Add(-80*time.Millisecond).UnixMilli(), 7), annexB...)
			if conn.Write(ctx, websocket.MessageBinary, frame) != nil {
				return
			}
			select {
			case <-periodic:
			case <-ctx.Done():
				return
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
	}))
	defer server.Close()
	var metrics []MediaTiming
	var mutex sync.Mutex
	var calibratedOnce sync.Once
	event := func(event Event) {
		if event.Stage == "camera_clock_reply" && event.Capabilities["accepted"] {
			calibratedOnce.Do(func() { close(calibrated) })
		}
		if event.Stage == "camera_frame_timing" && event.MediaTiming != nil {
			mutex.Lock()
			metrics = append(metrics, *event.MediaTiming)
			mutex.Unlock()
		}
	}
	if err := cameraVideo(ctx, &Session{Token: "fixture-token"}, server.URL, CameraVideoOptions{FirstUse: true, Authorized: true, EnableTiming: true}, &cameraOutput{}, event); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(metrics) != 1 || metrics[0].ClockValid || metrics[0].EncodeMS != 7 || metrics[0].ArrivalAgeMS != nil || metrics[0].ArrivalDriftMS != nil {
		t.Fatalf("invalid camera timing result: %+v", metrics)
	}
	data, _ := json.Marshal(metrics)
	if bytes.Contains(data, []byte("private")) || bytes.Contains(data, []byte("t1")) || bytes.Contains(data, []byte("fixture-token")) {
		t.Fatal("camera timing exposed private fields")
	}
}

func TestCameraVideoRequiresPermissionAndStopsOnCancellation(t *testing.T) {
	if err := CameraVideo(context.Background(), &Session{Token: "fixture"}, netip.MustParseAddr("10.1.2.3"), CameraVideoOptions{}, &cameraOutput{}, nil); err == nil {
		t.Fatal("camera opened without authorization")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if r.URL.Path == "/camera_share/screen" {
			_, _, _ = conn.Read(ctx)
			close(started)
		}
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- cameraVideo(ctx, &Session{Token: "fixture"}, server.URL, CameraVideoOptions{FirstUse: true, Authorized: true}, &cameraOutput{}, nil)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("camera stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("camera cancellation lost", err)
		}
	case <-time.After(time.Second):
		t.Fatal("camera cancellation did not join readers")
	}
}

type blockedCameraOutput struct {
	net.Conn
	started chan struct{}
}

func (w *blockedCameraOutput) Write(data []byte) (int, error) {
	close(w.started)
	return w.Conn.Write(data)
}

func TestCameraCancellationInterruptsBlockedOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if r.URL.Path == "/camera_share/screen" {
			_, _, _ = conn.Read(ctx)
			_ = conn.Write(ctx, websocket.MessageText, []byte(`DEVICE_INFO:{"mime_type":"video/avc","sessionId":0,"video_width":1280,"video_height":720}`))
			_ = conn.Write(ctx, websocket.MessageBinary, []byte("FRAME:\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x67"))
		}
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	output := &blockedCameraOutput{Conn: writer, started: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- cameraVideo(ctx, &Session{Token: "fixture"}, server.URL, CameraVideoOptions{FirstUse: true, Authorized: true}, output, nil)
	}()
	select {
	case <-output.started:
	case <-time.After(2 * time.Second):
		t.Fatal("camera never reached the blocked output")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("blocked output cancellation was not propagated", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt blocked camera output")
	}
}
