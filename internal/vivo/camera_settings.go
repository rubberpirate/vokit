package vivo

import (
	"encoding/json"
	"errors"
)

// CameraStreamCommand updates the active camera profile using only a stream
// size and frame rate advertised by the phone. The other profile, selected lens
// and unrelated configuration remain intact. The caller sends the returned
// command through the authorized main control channel.
// Shipped 127.js: resolution/frame-rate dictionaries near offset 165343,
// capability filtering near 653700, active-profile merge near 662544.
func CameraStreamCommand(config, capabilities json.RawMessage, width, height, fps, bitRate int) ([]byte, error) {
	resolutionOK := (width == 1280 && height == 720) || (width == 1920 && height == 1080) || (width == 3840 && height == 2160)
	fpsOK := fps == 25 || fps == 30 || fps == 50 || fps == 60
	if !resolutionOK || !fpsOK || bitRate < 0 || bitRate > 2 {
		return nil, errors.New("unsupported camera stream settings")
	}
	if width == 3840 && fps > 30 {
		return nil, errors.New("4K camera streaming above 30 fps is unavailable over LAN")
	}
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
	configKey, capabilityKey := "normalCameraConfig", "normalCameraCapability"
	if mode == 2 {
		configKey, capabilityKey = "liveCameraConfig", "liveCameraCapability"
	}
	var advertised struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streamInfo"`
		FPS []int `json:"fpsRange"`
	}
	if json.Unmarshal(caps[capabilityKey], &advertised) != nil || len(advertised.Streams) == 0 || len(advertised.Streams) > 128 || len(advertised.FPS) == 0 || len(advertised.FPS) > 32 {
		return nil, errors.New("invalid camera stream capabilities")
	}
	hasResolution, hasFPS := false, false
	for _, stream := range advertised.Streams {
		if stream.Width <= 0 || stream.Height <= 0 || stream.Width > 8192 || stream.Height > 8192 {
			return nil, errors.New("invalid camera stream capabilities")
		}
		hasResolution = hasResolution || stream.Width == width && stream.Height == height
	}
	for _, rate := range advertised.FPS {
		if rate <= 0 || rate > 240 {
			return nil, errors.New("invalid camera frame-rate capabilities")
		}
		hasFPS = hasFPS || rate == fps
	}
	if !hasResolution || !hasFPS {
		return nil, errors.New("requested camera stream settings are not advertised by the phone")
	}
	selected, err := cameraObject(settings[configKey])
	if err != nil {
		return nil, err
	}
	selected["streamInfo"], _ = json.Marshal(struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}{width, height})
	selected["frameRate"], _ = json.Marshal(fps)
	selected["bitRate"], _ = json.Marshal(bitRate)
	settings[configKey], _ = json.Marshal(selected)
	body, err := json.Marshal(settings)
	if err != nil || len(body) > maxResponseBytes-len("PC_UPDATE_CAMERA_PARA:") {
		return nil, errors.New("camera configuration exceeds size limit")
	}
	return append([]byte("PC_UPDATE_CAMERA_PARA:"), body...), nil
}
