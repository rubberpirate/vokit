package vivo

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func streamCameraFixtures(mode int) (json.RawMessage, json.RawMessage) {
	config, _ := json.Marshal(map[string]any{"cameraShareMode": mode, "private": "preserve config", "version": 11,
		"normalCameraConfig": map[string]any{"cameraType": "Back", "frameRate": 25, "bitRate": 2, "streamInfo": map[string]int{"width": 1920, "height": 1080}, "zoomRatio": 1.5, "videoMirrored": true},
		"liveCameraConfig":   map[string]any{"cameraType": "Tele3P5x", "frameRate": 50, "bitRate": 1, "streamInfo": map[string]int{"width": 1280, "height": 720}, "unknown": "retain"}})
	caps := json.RawMessage(`{"normalCameraCapability":{"streamInfo":[{"width":1280,"height":720},{"width":1920,"height":1080},{"width":3840,"height":2160}],"fpsRange":[25,30,50,60]},"liveCameraCapability":{"streamInfo":[{"width":1280,"height":720},{"width":1920,"height":1080},{"width":3840,"height":2160}],"fpsRange":[25,30,50,60]}}`)
	return config, caps
}

func TestCameraStreamCommandUpdatesOnlyActiveProfile(t *testing.T) {
	for _, mode := range []int{1, 2} {
		config, caps := streamCameraFixtures(mode)
		command, err := CameraStreamCommand(config, caps, 1280, 720, 30, 0)
		if err != nil || !bytes.HasPrefix(command, []byte("PC_UPDATE_CAMERA_PARA:")) {
			t.Fatal("stream command missing", err)
		}
		before, err := cameraObject(config)
		if err != nil {
			t.Fatal(err)
		}
		after, err := cameraObject(command[len("PC_UPDATE_CAMERA_PARA:"):])
		if err != nil {
			t.Fatal(err)
		}
		active, other := "normalCameraConfig", "liveCameraConfig"
		if mode == 2 {
			active, other = other, active
		}
		for _, key := range []string{"private", "version", "cameraShareMode", other} {
			if !bytes.Equal(before[key], after[key]) {
				t.Fatalf("untouched config field changed: %s", key)
			}
		}
		profile, _ := cameraObject(after[active])
		original, _ := cameraObject(before[active])
		for key, value := range original {
			if key != "frameRate" && key != "streamInfo" && key != "bitRate" && !bytes.Equal(value, profile[key]) {
				t.Fatalf("selected lens or config field changed: %s", key)
			}
		}
		if string(profile["frameRate"]) != "30" || string(profile["bitRate"]) != "0" {
			t.Fatal("stream numeric settings not applied")
		}
		var selected struct{ Width, Height int }
		if json.Unmarshal(profile["streamInfo"], &selected) != nil || selected.Width != 1280 || selected.Height != 720 {
			t.Fatal("resolution not applied")
		}
	}
}

func TestCameraStreamCommandRequiresSupportedAdvertisedResolutionAndFPS(t *testing.T) {
	config, caps := streamCameraFixtures(1)
	for _, preset := range [][4]int{{1280, 720, 25, 0}, {1920, 1080, 60, 2}, {3840, 2160, 30, 1}} {
		if _, err := CameraStreamCommand(config, caps, preset[0], preset[1], preset[2], preset[3]); err != nil {
			t.Fatal("supported preset rejected", err)
		}
	}
	for _, preset := range [][4]int{{0, 720, 30, 0}, {720, 1280, 30, 0}, {640, 480, 30, 0}, {8192, 8192, 30, 0}, {1280, 720, 24, 0}, {1280, 720, 120, 0}, {1280, 720, 30, -1}, {1280, 720, 30, 3}, {3840, 2160, 50, 0}, {3840, 2160, 60, 0}} {
		if _, err := CameraStreamCommand(config, caps, preset[0], preset[1], preset[2], preset[3]); err == nil {
			t.Errorf("unsupported preset accepted: %v", preset)
		}
	}
	for _, capability := range []string{
		`{"streamInfo":[{"width":1920,"height":1080}],"fpsRange":[30]}`,
		`{"streamInfo":[{"width":1280,"height":720}],"fpsRange":[25]}`,
		`{"streamInfo":[{"width":1280,"height":720}],"fpsRange":"30"}`,
		`{"streamInfo":[{"width":"1280","height":720}],"fpsRange":[30]}`,
		`{"streamInfo":[{"width":1280,"height":720}],"fpsRange":["30"]}`,
		`{"streamInfo":[{"width":1280,"height":720}],"fpsRange":[30.5]}`,
		`{"streamInfo":[{"width":1280,"height":720},{"width":-1,"height":0}],"fpsRange":[30]}`,
		`null`, `{}`, `{"streamInfo":null,"fpsRange":null}`,
	} {
		if _, err := CameraStreamCommand(config, []byte(`{"normalCameraCapability":`+capability+`}`), 1280, 720, 30, 0); err == nil {
			t.Fatal("unsupported or malformed capabilities accepted")
		}
	}
}

func TestCameraStreamCommandBoundsAndRejectsMissingModeProfile(t *testing.T) {
	config, caps := streamCameraFixtures(1)
	for _, cfg := range []string{`null`, `{}`, `{"cameraShareMode":3}`, `{"cameraShareMode":1,"normalCameraConfig":null}`, `{"cameraShareMode":2,"normalCameraConfig":{}}`} {
		if _, err := CameraStreamCommand([]byte(cfg), caps, 1280, 720, 30, 0); err == nil {
			t.Fatal("invalid mode or active profile accepted")
		}
	}
	oversized := []byte(`{"normalCameraCapability":{"streamInfo":[{"width":1280,"height":720}],"fpsRange":[30],"private":"` + strings.Repeat("x", maxResponseBytes) + `"}}`)
	if _, err := CameraStreamCommand(config, oversized, 1280, 720, 30, 0); err == nil {
		t.Fatal("oversized capability accepted")
	}
	var c map[string]json.RawMessage
	_ = json.Unmarshal(caps, &c)
	var active map[string]json.RawMessage
	_ = json.Unmarshal(c["normalCameraCapability"], &active)
	active["fpsRange"] = []byte("[" + strings.TrimSuffix(strings.Repeat("30,", 33), ",") + "]")
	c["normalCameraCapability"], _ = json.Marshal(active)
	many, _ := json.Marshal(c)
	if _, err := CameraStreamCommand(config, many, 1280, 720, 30, 0); err == nil {
		t.Fatal("oversized FPS list accepted")
	}
}
