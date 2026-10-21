package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vivolinux/internal/service"
	"vivolinux/internal/vivo"
)

type nativeMedia struct {
	session         *vivo.Session
	phone           netip.Addr
	firstUse        bool
	pin             string
	connected       func() bool
	requests        chan []byte
	mirrorAuth      chan bool
	mirrorAllowed   atomic.Bool
	camera          vivo.CameraState
	cameraMutex     sync.Mutex
	cameraConfigSeq atomic.Uint64
	cameraAuthSeq   atomic.Uint64
	cameraChanged   chan struct{}
	stopLegacy      context.CancelFunc
	legacyDone      <-chan struct{}
	event           func(vivo.Event)
}

func newNativeMedia(s *vivo.Session, phone netip.Addr, firstUse bool, pin string, connected func() bool, event func(vivo.Event)) *nativeMedia {
	return &nativeMedia{session: s, phone: phone, firstUse: firstUse, pin: pin, connected: connected, requests: make(chan []byte, 16), mirrorAuth: make(chan bool, 4), cameraChanged: make(chan struct{}, 1), event: event}
}
func (m *nativeMedia) mirrorPermission(allowed bool) {
	m.mirrorAllowed.Store(allowed)
	select {
	case m.mirrorAuth <- allowed:
	default:
	}
}
func (m *nativeMedia) cameraMessage(data []byte) {
	m.cameraMutex.Lock()
	handled, err := m.camera.ApplyMessage(data)
	if !handled || err != nil {
		m.cameraMutex.Unlock()
		return
	}
	if bytes.HasPrefix(data, []byte("PC_RESPONSE_CAMERA_AUTH:")) {
		m.cameraAuthSeq.Add(1)
	}
	if bytes.HasPrefix(data, []byte("PC_GET_CAMERA_CONFIG:")) {
		m.cameraConfigSeq.Add(1)
	}
	m.cameraMutex.Unlock()
	select {
	case m.cameraChanged <- struct{}{}:
	default:
	}
}
func (m *nativeMedia) send(ctx context.Context, data []byte) error {
	if len(data) == 0 || len(data) > 16*1024 {
		return errors.New("invalid media control request")
	}
	select {
	case m.requests <- data:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *nativeMedia) serve(ctx context.Context, request service.MediaRequest, channel *service.MediaChannel) error {
	if !m.connected() {
		return errors.New("phone is disconnected")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if m.stopLegacy != nil {
		m.stopLegacy()
		select {
		case <-m.legacyDone:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
			return errors.New("previous preview is stopping")
		}
	}
	_ = channel.Info(map[string]string{"source": request.Source, "stage": "permission_wait"})
	permissionCtx, done := context.WithTimeout(ctx, 60*time.Second)
	defer done()
	if request.Source == "screen" {
		for len(m.mirrorAuth) > 0 {
			<-m.mirrorAuth
		}
		if err := m.send(permissionCtx, []byte("req_authrity")); err != nil {
			return err
		}
		select {
		case allowed := <-m.mirrorAuth:
			if !allowed {
				return errors.New("screen permission denied")
			}
		case <-permissionCtx.Done():
			return permissionCtx.Err()
		}
	} else {
		if err := m.authorizeCamera(permissionCtx, request.Facing, request.Quality); err != nil {
			if errors.Is(err, errCameraQualityUnavailable) {
				_ = channel.Packet(service.MediaStatus, []byte(`{"error":"Selected camera quality is unavailable. Choose Phone default and retry."}`))
				return nil
			}
			return err
		}
	}
	_ = channel.Info(map[string]string{"source": request.Source, "stage": "starting"})
	input := make(chan vivo.InputEvent, 16)
	inputDone := make(chan struct{})
	go func() {
		defer close(inputDone)
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if request.Source == "screen" && !m.mirrorAllowed.Load() {
					return
				}
				if request.Source == "camera" {
					state := m.camera.Snapshot()
					if !state.ServerEnabled || (state.AuthorizationKnown && !state.Authorized) {
						return
					}
				}
			case data := <-channel.Input:
				var kind struct {
					Kind string `json:"kind"`
				}
				if json.Unmarshal(data, &kind) != nil {
					return
				}
				if kind.Kind == "camera" {
					if request.Source != "camera" {
						return
					}
					var change struct {
						Kind   string `json:"kind"`
						Facing string `json:"facing"`
					}
					if decodeMediaInput(data, &change) != nil || (change.Facing != "front" && change.Facing != "rear") {
						return
					}
					state := m.camera.Snapshot()
					command, err := vivo.CameraSwitchCommand(state.Config, state.Capabilities, change.Facing)
					if err == nil {
						command, err = cameraQualityCommand(command, state.Capabilities, request.Quality)
					}
					if err != nil || m.send(ctx, command) != nil {
						return
					}
					continue
				}
				if request.Source != "screen" {
					continue
				}
				var event vivo.InputEvent
				if decodeMediaInput(data, &event) != nil {
					return
				}
				if _, err := vivo.EncodeInput(event, 0); err != nil {
					return
				}
				select {
				case input <- event:
				case <-ctx.Done():
					return
				default:
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-inputDone }()
	if request.Source == "camera" {
		options := vivo.CameraVideoOptions{FirstUse: m.firstUse, Pin: m.pin, Authorized: true, EnableTiming: true, AudioProbe: request.AudioProbe}
		if request.Microphone {
			options.Audio = func(data []byte) error { return channel.Packet(service.MediaAudio, data) }
			options.AudioCapability = func(available bool) {
				_ = channel.Info(map[string]any{"microphone_available": available})
			}
		}
		return vivo.CameraVideo(ctx, m.session, m.phone, options, channel, m.event)
	}
	return vivo.MirrorInteractive(ctx, m.session, m.phone, m.firstUse, m.pin, channel, vivo.MirrorOptions{Input: input, MaxSize: 1920, FrameRate: 60, EnableTiming: true, DeviceInfo: func(info vivo.MirrorInfo) { _ = channel.Info(info) }}, m.event)
}
func decodeMediaInput(data []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid media input")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid media input")
	}
	return nil
}

func (m *nativeMedia) cameraSnapshot() (vivo.CameraSnapshot, uint64, uint64) {
	m.cameraMutex.Lock()
	defer m.cameraMutex.Unlock()
	return m.camera.Snapshot(), m.cameraAuthSeq.Load(), m.cameraConfigSeq.Load()
}

var errCameraQualityUnavailable = errors.New("selected camera quality unavailable")

func cameraQualityCommand(command []byte, capabilities json.RawMessage, quality string) ([]byte, error) {
	if quality == "" {
		return command, nil
	}
	width, height, rate := 1280, 720, 0
	if quality == "1080p" {
		width, height, rate = 1920, 1080, 1
	} else if quality != "720p" {
		return nil, errCameraQualityUnavailable
	}
	updated, err := vivo.CameraStreamCommand(command[len("PC_UPDATE_CAMERA_PARA:"):], capabilities, width, height, 30, rate)
	if err != nil {
		return nil, errCameraQualityUnavailable
	}
	return updated, nil
}

func (m *nativeMedia) authorizeCamera(ctx context.Context, facing string, quality ...string) error {
	if len(quality) > 1 {
		return errCameraQualityUnavailable
	}
	if err := m.send(ctx, vivo.CameraConfigRequest()); err != nil {
		return err
	}
	state, _, _ := m.cameraSnapshot()
	if !state.ServerEnabled {
		if err := m.send(ctx, vivo.CameraEnableRequest()); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, _, _ = m.cameraSnapshot()
		if state.ServerEnabled && len(state.Config) > 0 && len(state.Capabilities) > 0 {
			break
		}
		select {
		case <-m.cameraChanged:
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	mode, err := vivo.CameraShareMode(state.Config)
	if err != nil {
		return err
	}
	permission, err := vivo.CameraPermissionRequest("vivo Office Kit", mode)
	if err != nil {
		return err
	}
	_, before, _ := m.cameraSnapshot()
	if err := m.send(ctx, permission); err != nil {
		return err
	}
	for {
		var authSeq uint64
		state, authSeq, _ = m.cameraSnapshot()
		if authSeq > before && state.AuthorizationKnown {
			if !state.Authorized {
				return errors.New("camera permission denied")
			}
			break
		}
		select {
		case <-m.cameraChanged:
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, _, configBefore := m.cameraSnapshot()
	if err := m.send(ctx, vivo.CameraConfigRequest()); err != nil {
		return err
	}
	for {
		var configSeq uint64
		state, _, configSeq = m.cameraSnapshot()
		if configSeq > configBefore {
			break
		}
		select {
		case <-m.cameraChanged:
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	currentMode, err := vivo.CameraShareMode(state.Config)
	if err != nil || currentMode != mode || !state.Authorized || !state.ServerEnabled {
		return errors.New("camera state changed during authorization")
	}
	command, err := vivo.CameraSwitchCommand(state.Config, state.Capabilities, facing)
	if err != nil {
		return err
	}
	if len(quality) == 1 && quality[0] != "" {
		command, err = cameraQualityCommand(command, state.Capabilities, quality[0])
		if err != nil {
			return err
		}
	}
	return m.send(ctx, command)
}
