package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"vivolinux/internal/vivo"
)

func cameraFixture(m *nativeMedia, mode int) {
	m.cameraMessage([]byte(fmt.Sprintf(`PC_GET_CAMERA_CONFIG:{"cameraShareMode":%d,"normalCameraConfig":{"cameraType":"frontMaster","preserved":7},"liveCameraConfig":{"cameraType":"frontMaster","preserved":8}}`, mode)))
	m.cameraMessage([]byte(`PC_GET_CAMERA_CAPABILITY:{"normalCameraCapability":{"cameraInfos":["frontMaster","Back"]},"liveCameraCapability":{"cameraInfos":["frontMaster","Master"]}}`))
}

func TestCameraQualityKeepsLensAndUsesAdvertisedPreset(t *testing.T) {
	for _, mode := range []int{1, 2} {
		m := newNativeMedia(&vivo.Session{}, netip.MustParseAddr("10.1.2.3"), true, "", func() bool { return true }, nil)
		cameraFixture(m, mode)
		m.cameraMessage([]byte(`PC_GET_CAMERA_CAPABILITY:{"normalCameraCapability":{"cameraInfos":["frontMaster","Back"],"streamInfo":[{"width":1280,"height":720},{"width":1920,"height":1080}],"fpsRange":[30]},"liveCameraCapability":{"cameraInfos":["frontMaster","Master"],"streamInfo":[{"width":1280,"height":720},{"width":1920,"height":1080}],"fpsRange":[30]}}`))
		state := m.camera.Snapshot()
		lens, err := vivo.CameraSwitchCommand(state.Config, state.Capabilities, "rear")
		if err != nil {
			t.Fatal(err)
		}
		for _, quality := range []string{"720p", "1080p"} {
			command, err := cameraQualityCommand(lens, state.Capabilities, quality)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]json.RawMessage
			if json.Unmarshal(command[len("PC_UPDATE_CAMERA_PARA:"):], &cfg) != nil {
				t.Fatal("bad configuration")
			}
			key, rear := "normalCameraConfig", "Back"
			if mode == 2 {
				key, rear = "liveCameraConfig", "Master"
			}
			var selected struct {
				CameraType         string
				FrameRate, BitRate int
				StreamInfo         struct{ Width, Height int }
				Preserved          int
			}
			if json.Unmarshal(cfg[key], &selected) != nil {
				t.Fatal("bad camera settings")
			}
			wantWidth, wantRate := 1280, 0
			if quality == "1080p" {
				wantWidth, wantRate = 1920, 1
			}
			if selected.CameraType != rear || selected.StreamInfo.Width != wantWidth || selected.FrameRate != 30 || selected.BitRate != wantRate || selected.Preserved == 0 {
				t.Fatal("preset changed lens or unrelated configuration")
			}
		}
	}
}
func TestCameraAuthorizationPreservesNegotiatedModeAndRefreshesConfig(t *testing.T) {
	for _, mode := range []int{1, 2} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			m := newNativeMedia(&vivo.Session{Token: "fixture"}, netip.MustParseAddr("10.1.2.3"), true, "", func() bool { return true }, nil)
			cameraFixture(m, mode)
			m.cameraMessage([]byte(`PC_GET_CAMERA_STATUS:{"state":1}`))
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			updated := make(chan []byte, 1)
			done := make(chan struct{})
			var configs atomic.Int32
			go func() {
				defer close(done)
				for {
					select {
					case <-ctx.Done():
						return
					case request := <-m.requests:
						switch {
						case string(request) == "PC_GET_CAMERA_CONFIG:":
							configs.Add(1)
							cameraFixture(m, mode)
						case strings.HasPrefix(string(request), "PC_REQUEST_CAMERA_AUTH:"):
							var body struct {
								Mode int    `json:"share_mode"`
								From string `json:"start_from"`
							}
							json.Unmarshal(request[len("PC_REQUEST_CAMERA_AUTH:"):], &body)
							if body.Mode != mode || body.From != "vivo Office Kit" {
								t.Error("permission mode or source changed")
							}
							m.cameraMessage([]byte(`PC_RESPONSE_CAMERA_AUTH:{"auth":1}`))
						case strings.HasPrefix(string(request), "PC_UPDATE_CAMERA_PARA:"):
							updated <- request
						}
					}
				}
			}()
			if err := m.authorizeCamera(ctx, "rear"); err != nil {
				t.Fatal(err)
			}
			select {
			case request := <-updated:
				rear := "Back"
				if mode == 2 {
					rear = "Master"
				}
				if !strings.Contains(string(request), `"cameraType":"`+rear+`"`) || !strings.Contains(string(request), `"preserved":`) {
					t.Fatal("switch lost configuration or selected wrong rear camera")
				}
			case <-ctx.Done():
				t.Fatal("no camera switch sent")
			}
			if configs.Load() < 2 {
				t.Fatal("configuration not refreshed after approval")
			}
			cancel()
			<-done
		})
	}
}
func TestCameraApprovalDuringEnableDoesNotApproveLaterRequest(t *testing.T) {
	m := newNativeMedia(&vivo.Session{}, netip.MustParseAddr("10.1.2.3"), true, "", func() bool { return true }, nil)
	cameraFixture(m, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	var switches, requests atomic.Int32
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-m.requests:
				switch {
				case string(request) == "PC_GET_CAMERA_CONFIG:":
					cameraFixture(m, 1)
				case string(request) == "PC_ENABLE_CAMERA_AUTH:":
					m.cameraMessage([]byte(`PC_RESPONSE_CAMERA_AUTH:{"auth":1}`))
					m.cameraMessage([]byte(`PC_GET_CAMERA_STATUS:{"state":1}`))
				case strings.HasPrefix(string(request), "PC_REQUEST_CAMERA_AUTH:"):
					requests.Add(1)
				case strings.HasPrefix(string(request), "PC_UPDATE_CAMERA_PARA:"):
					switches.Add(1)
				}
			}
		}
	}()
	err := m.authorizeCamera(ctx, "rear")
	cancel()
	<-done
	if !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 1 || switches.Load() != 0 {
		t.Fatal("earlier approval authorized a later request", err)
	}
}
