package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"vivolinux/internal/remotepc"
	"vivolinux/internal/vivo"
)

type remotePCOptions struct{ stateDir, helper, country string }
type remotePCOutput struct {
	Event             string `json:"event"`
	State             string `json:"state,omitempty"`
	Message           string `json:"message,omitempty"`
	Error             string `json:"error,omitempty"`
	DeviceCode        string `json:"device_code,omitempty"`
	TemporaryPassword string `json:"temporary_password,omitempty"`
}

func parseRemotePCOptions(args []string) (remotePCOptions, error) {
	var options remotePCOptions
	flags := flag.NewFlagSet("remote-pc", flag.ContinueOnError)
	flags.StringVar(&options.stateDir, "state-dir", "build/remote-pc", "private native Remote PC state")
	flags.StringVar(&options.helper, "desktop-helper", "", "desktop helper path (default beside executable)")
	flags.StringVar(&options.country, "country", "IN", "two-letter Office Kit country code")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 || len(options.country) != 2 || options.country[0] < 'A' || options.country[0] > 'Z' || options.country[1] < 'A' || options.country[1] > 'Z' || options.stateDir == "" || strings.ContainsRune(options.stateDir, 0) || strings.ContainsRune(options.helper, 0) {
		return options, errors.New("remote-pc requires valid flags and an uppercase two-letter country")
	}
	if options.helper == "" {
		executable, err := os.Executable()
		if err != nil {
			return options, errors.New("cannot locate desktop helper")
		}
		options.helper = filepath.Join(filepath.Dir(executable), "vivo-desktop-helper")
	}
	return options, nil
}

type remoteStoredState struct {
	Country        string `json:"country"`
	DeviceNo       string `json:"deviceNo"`
	DeviceToken    string `json:"deviceToken"`
	PriDeviceToken string `json:"priDeviceToken"`
}

func remotePrivate(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && info.Mode().Perm()&0077 == 0 && ((directory && info.IsDir()) || (!directory && info.Mode().IsRegular()))
}

func openRemoteState(dir, country string) (vivo.Identity, remoteStoredState, *os.File, error) {
	var stored remoteStoredState
	id, err := vivo.LoadIdentity(dir)
	if err != nil {
		return id, stored, nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "session.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return id, stored, nil, errors.New("cannot open private Remote PC session lock")
	}
	info, err := lock.Stat()
	if err != nil || !remotePrivate(info, false) || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return id, stored, nil, errors.New("Remote PC is already running or its state lock is not private")
	}
	path := filepath.Join(dir, "device.json")
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return id, stored, lock, nil
	}
	if err != nil {
		lock.Close()
		return id, stored, nil, errors.New("cannot read private Remote PC state")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !remotePrivate(info, false) {
		lock.Close()
		return id, stored, nil, errors.New("Remote PC state must be a private user-owned regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 32769))
	if err != nil || len(raw) > 32768 || json.Unmarshal(raw, &stored) != nil || stored.Country != country {
		lock.Close()
		return id, remoteStoredState{}, nil, errors.New("invalid Remote PC state or country mismatch; choose another state directory")
	}
	for _, v := range []string{stored.DeviceNo, stored.DeviceToken, stored.PriDeviceToken} {
		if len(v) > 8192 || strings.ContainsAny(v, "\r\n\x00") {
			lock.Close()
			return id, remoteStoredState{}, nil, errors.New("invalid private Remote PC state")
		}
	}
	return id, stored, lock, nil
}

func saveRemoteState(dir, country string, device remotepc.Device) error {
	if info, err := os.Lstat(dir); err != nil || !remotePrivate(info, true) {
		return errors.New("Remote PC state directory is not private")
	}
	path := filepath.Join(dir, "device.json")
	if info, err := os.Lstat(path); err == nil {
		if !remotePrivate(info, false) {
			return errors.New("Remote PC state file is not private")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect Remote PC state")
	}
	f, err := os.CreateTemp(dir, ".device-*")
	if err != nil {
		return errors.New("cannot create private Remote PC state")
	}
	temp := f.Name()
	defer os.Remove(temp)
	value := remoteStoredState{country, device.DeviceNo, device.DeviceToken, device.PriDeviceToken}
	if err := json.NewEncoder(f).Encode(value); err != nil {
		f.Close()
		return errors.New("cannot encode Remote PC state")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errors.New("cannot persist Remote PC state")
	}
	if err := f.Close(); err != nil {
		return errors.New("cannot close Remote PC state")
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.New("cannot replace private Remote PC state")
	}
	return nil
}

type remoteHostEvent struct {
	Event         string  `json:"event"`
	Stage         string  `json:"stage"`
	SDP           string  `json:"sdp"`
	Control       bool    `json:"control"`
	Error         string  `json:"error"`
	CaptureWidth  int     `json:"capture_width"`
	CaptureHeight int     `json:"capture_height"`
	ScreenX       int     `json:"screen_x"`
	ScreenY       int     `json:"screen_y"`
	ScreenScale   float64 `json:"screen_scale"`
	ScreenLocked  *bool   `json:"screen_locked"`
	CaptureFrames int64   `json:"capture_frames"`
	RTPPackets    int64   `json:"rtp_packets"`
}
type remoteDesktop interface {
	Send(any) error
	Events() <-chan remoteHostEvent
	Close()
}
type remoteHelper struct {
	input   io.WriteCloser
	encoder *json.Encoder
	mu      sync.Mutex
	events  chan remoteHostEvent
	done    chan struct{}
	cancel  context.CancelFunc
}

func startRemoteHelper(path string) (*remoteHelper, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, path, "--remote-host")
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errors.New("cannot create Remote PC helper input")
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		cancel()
		return nil, errors.New("cannot create Remote PC helper output")
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		input.Close()
		cancel()
		return nil, errors.New("cannot start Remote PC desktop helper")
	}
	h := &remoteHelper{input: input, encoder: json.NewEncoder(input), events: make(chan remoteHostEvent, 16), done: make(chan struct{}), cancel: cancel}
	go func() { defer close(h.done); _ = cmd.Wait() }()
	go func() {
		defer close(h.events)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			var event remoteHostEvent
			if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Event == "" {
				event = remoteHostEvent{Event: "error"}
			}
			if event.Event == "input" {
				continue
			}
			select {
			case h.events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return h, nil
}
func (h *remoteHelper) Send(value any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.encoder.Encode(value) != nil {
		return errors.New("cannot communicate with Remote PC desktop helper")
	}
	return nil
}
func (h *remoteHelper) Events() <-chan remoteHostEvent { return h.events }
func (h *remoteHelper) Close() {
	_ = h.Send(map[string]any{"op": "stop"})
	h.input.Close()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.cancel()
		<-h.done
	}
	h.cancel()
}

type remoteSIP interface {
	SendResponse(context.Context, remotepc.SIPMessage, int, string, []remotepc.SIPHeader, []byte) error
	Done() <-chan struct{}
	Err() error
	Close() error
}
type remoteSignal interface {
	Read(context.Context) (remotepc.SignalMessage, error)
	Send(context.Context, remotepc.SignalMessage) error
	EndSession(context.Context) error
	Close()
}
type remoteBackend struct {
	region  remotepc.Region
	account remotepc.SIPAccount
	sip     remoteSIP
	signal  remoteSignal
}
type remotePCEvent struct {
	stage    string
	err      error
	device   *remotepc.Device
	backend  *remoteBackend
	sip      *remotepc.SIPMessage
	signal   *remotepc.SignalMessage
	password string
}
type remotePCSession struct {
	ctx                                                    context.Context
	output                                                 *json.Encoder
	helper                                                 remoteDesktop
	cloud                                                  *remotepc.Client
	region                                                 remotepc.Region
	device                                                 remotepc.Device
	account                                                remotepc.SIPAccount
	sip                                                    remoteSIP
	signal                                                 remoteSignal
	captureReady, sipReady, signalReady, control, rotating bool
	readySent                                              bool
	peer, connection                                       string
	crypto                                                 *remotepc.RTCCrypto
	invite                                                 *remotepc.SIPMessage
	answer                                                 string
	connected                                              bool
	sessionDeadline                                        time.Time
	diagnostic                                             io.Writer
	diagnosticLines                                        int
	screen                                                 remoteHostEvent
	screenMessage                                          *remotepc.SignalMessage
	screenAttempts                                         int
	screenRetryAt                                          time.Time
}

func (s *remotePCSession) trace(stage string) {
	if s.diagnostic != nil && s.diagnosticLines < 256 {
		fmt.Fprintln(s.diagnostic, time.Now().UTC().Format(time.RFC3339), stage)
		s.diagnosticLines++
	}
}

type remotePCDependencies struct {
	startHelper func(string) (remoteDesktop, error)
	bootstrap   func(context.Context, remotePCOptions, vivo.Identity, remoteStoredState, *remotepc.Client, func(remotePCEvent) bool)
}

func (s *remotePCSession) emit(value remotePCOutput) error {
	if value.Event == "error" && value.Error == "" {
		value.Error = value.Message
	}
	return s.output.Encode(value)
}
func (s *remotePCSession) status(state, message string) error {
	s.trace("status " + state + ": " + message)
	return s.emit(remotePCOutput{Event: "status", State: state, Message: message})
}
func (s *remotePCSession) ready() bool {
	return s.captureReady && s.sipReady && s.signalReady && !s.rotating
}
func (s *remotePCSession) reportReady() error {
	if !s.ready() || s.readySent {
		return nil
	}
	s.readySent = true
	message := "Ready. Open Remote PC on your phone and enter this device code and password."
	if !s.control {
		message = "Ready to view. Input control is unavailable for this desktop session."
	}
	return s.status("ready", message)
}

func (s *remotePCSession) endSession(notify bool) error {
	had := s.peer != "" || s.invite != nil || s.connected
	s.peer, s.connection, s.answer = "", "", ""
	s.crypto = nil
	s.invite = nil
	s.connected = false
	s.sessionDeadline = time.Time{}
	s.screenMessage, s.screenAttempts, s.screenRetryAt = nil, 0, time.Time{}
	if had {
		s.captureReady = false
		s.readySent = false
		if err := s.helper.Send(map[string]any{"op": "reset"}); err != nil {
			return err
		}
		if notify && s.signal != nil {
			if err := s.signal.EndSession(s.ctx); err != nil {
				return err
			}
		}
		if err := s.emit(remotePCOutput{Event: "disconnected", Message: "The phone disconnected."}); err != nil {
			return err
		}
	}
	return nil
}

func (s *remotePCSession) open(message remotepc.SignalMessage) error {
	s.trace(fmt.Sprintf("cloud open: ready=%t passwordType=%d connectionType=%d", s.ready(), message.Ability.PasswordType, message.ConnectionType))
	var peer string
	if !s.ready() || message.ConnectionType != 0 || message.Ability.PasswordType != 1 || s.device.TempPassword == "" || message.ConnectionID == "" || json.Unmarshal(message.Data, &peer) != nil || peer == "" || (s.peer != "" && (peer != s.peer || message.ConnectionID != s.connection)) {
		if s.peer != "" {
			return s.endSession(true)
		}
		if s.signal != nil {
			return s.signal.EndSession(s.ctx)
		}
		return nil
	}
	if s.peer == "" {
		digest := sha256.Sum256([]byte(s.device.TempPassword + "salt"))
		crypto, err := remotepc.NewRTCCrypto(hex.EncodeToString(digest[:]))
		if err != nil {
			return err
		}
		s.peer, s.connection, s.crypto = peer, message.ConnectionID, crypto
		s.sessionDeadline = time.Now().Add(45 * time.Second)
	}
	if s.screen.ScreenLocked != nil {
		if err := s.sendDesktopMessage(map[string]any{"type": "screenLockInfo", "isLock": *s.screen.ScreenLocked}); err != nil {
			return err
		}
	}
	data, err := json.Marshal(map[string]any{"commandTransport": "datachannel", "dcHeartBeat": 1, "p2pSecurityInfo": s.crypto.SecurityInfo()})
	if err != nil {
		return errors.New("cannot prepare Remote PC session security")
	}
	if err := s.signal.Send(s.ctx, remotepc.SignalMessage{Action: "openSuccess", ConnectionID: s.connection, TargetDeviceNo: s.peer, Data: data}); err != nil {
		return err
	}
	return s.status("connecting", "Phone authenticated. Waiting for its video connection.")
}

// Desktop notifications use the active authenticated cloud relationship. The
// source dimensions describe the shared display, not the encoded video size.
func (s *remotePCSession) desktopMessage(data any) (remotepc.SignalMessage, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return remotepc.SignalMessage{}, errors.New("cannot prepare desktop metadata")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return remotepc.SignalMessage{}, errors.New("cannot identify desktop metadata")
	}
	return remotepc.SignalMessage{Action: "msg", MsgID: hex.EncodeToString(id[:]), ConnectionID: s.connection, TargetDeviceNo: s.peer, Data: raw}, nil
}

func (s *remotePCSession) sendDesktopMessage(data any) error {
	message, err := s.desktopMessage(data)
	if err != nil {
		return err
	}
	return s.signal.Send(s.ctx, message)
}

func (s *remotePCSession) sendScreenInfo(now time.Time) error {
	if s.peer == "" || s.invite == nil || s.screen.CaptureWidth <= 0 || s.screen.CaptureWidth > 32768 || s.screen.CaptureHeight <= 0 || s.screen.CaptureHeight > 32768 {
		return nil
	}
	// The portal authorizes one selected source, represented by display id 0.
	message, err := s.desktopMessage(map[string]any{"type": "deviceScreenInfo", "currentScreenIndex": 0, "mScreenInfos": []any{map[string]any{"height": s.screen.CaptureHeight, "id": 0, "selected": true, "title": "Shared display", "width": s.screen.CaptureWidth, "x": s.screen.ScreenX, "y": s.screen.ScreenY, "index": 0}}})
	if err != nil {
		return err
	}
	if err := s.signal.Send(s.ctx, message); err != nil {
		return err
	}
	s.screenMessage, s.screenAttempts, s.screenRetryAt = &message, 1, now.Add(2*time.Second)
	s.trace("desktop screen metadata sent")
	// The wire value is fractional scaling (1.25), not percent (125).
	if scale := s.screen.ScreenScale; scale >= 0.25 && scale <= 8 && !math.IsNaN(scale) && !math.IsInf(scale, 0) {
		return s.sendDesktopMessage(map[string]any{"type": "deviceScreenDpiInfo", "value": scale})
	}
	return nil
}

func (s *remotePCSession) retryScreenInfo(now time.Time) error {
	if s.screenMessage == nil || now.Before(s.screenRetryAt) {
		return nil
	}
	if s.screenAttempts >= 3 {
		s.screenMessage = nil
		return nil
	}
	if err := s.signal.Send(s.ctx, *s.screenMessage); err != nil {
		return err
	}
	s.screenAttempts++
	s.screenRetryAt = now.Add(2 * time.Second)
	return nil
}

func (s *remotePCSession) confirmScreenInfo(message remotepc.SignalMessage) {
	if s.screenMessage == nil || message.Action != "confirm" || message.MsgID != s.screenMessage.MsgID || message.ConnectionID != s.connection || message.SourceDeviceNo != s.peer || message.TargetDeviceNo != s.device.DeviceNo {
		return
	}
	var data struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(message.Data, &data) == nil && data.Type == "deviceScreenInfo" {
		s.screenMessage = nil
		s.trace("desktop screen metadata confirmed")
	}
}

func remoteSIPUser(value string) string {
	if start := strings.IndexByte(value, '<'); start >= 0 {
		end := strings.IndexByte(value[start:], '>')
		if end < 0 {
			return ""
		}
		value = value[start+1 : start+end]
	} else {
		value = strings.SplitN(value, ";", 2)[0]
	}
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "sip:") {
		value = strings.TrimPrefix(value, "sip:")
	} else if strings.HasPrefix(value, "sips:") {
		value = strings.TrimPrefix(value, "sips:")
	} else {
		return ""
	}
	user, host, ok := strings.Cut(value, "@")
	if !ok || host == "" || strings.ContainsAny(user, " :<>\t\r\n") {
		return ""
	}
	return user
}

func (s *remotePCSession) authorized(request remotepc.SIPMessage) bool {
	return s.ready() && s.peer != "" && s.connection != "" && s.crypto != nil && remoteSIPUser(request.Header("From")) == s.peer && remoteSIPUser(request.Header("To")) == s.account.SIPUserName
}

func (s *remotePCSession) handleSIP(request remotepc.SIPMessage) error {
	method := request.Method()
	switch method {
	case "INVITE", "ACK", "CANCEL", "BYE", "MESSAGE", "OPTIONS":
		s.trace(fmt.Sprintf("sip %s: authorized=%t from_matches=%t to_matches=%t", method, s.authorized(request), remoteSIPUser(request.Header("From")) == s.peer, remoteSIPUser(request.Header("To")) == s.account.SIPUserName))
	}
	respond := func(code int, reason string) error { return s.sip.SendResponse(s.ctx, request, code, reason, nil, nil) }
	if request.Method() == "OPTIONS" {
		return respond(200, "OK")
	}
	if request.Method() == "ACK" {
		return nil
	}
	if !s.authorized(request) {
		return respond(403, "Forbidden")
	}
	switch request.Method() {
	case "INVITE":
		if s.invite != nil {
			if request.Header("Call-ID") == s.invite.Header("Call-ID") && request.Header("CSeq") == s.invite.Header("CSeq") {
				if s.answer != "" {
					return s.sendAnswer(request, s.answer)
				}
				return respond(100, "Trying")
			}
			return respond(486, "Busy Here")
		}
		if strings.ToLower(strings.TrimSpace(strings.SplitN(request.Header("Content-Type"), ";", 2)[0])) != "application/sdp" {
			return respond(415, "Unsupported Media Type")
		}
		sdp, err := s.crypto.DecryptSDP(string(request.Body))
		if err != nil {
			s.trace("sip offer: fingerprint authentication failed")
			return respond(488, "Not Acceptable Here")
		}
		s.trace("sip offer: fingerprints authenticated")
		if err := respond(100, "Trying"); err != nil {
			return err
		}
		copy := request
		s.invite = &copy
		if err := s.sendScreenInfo(time.Now()); err != nil {
			return err
		}
		stun := ""
		if s.account.STUNAddress != "" {
			stun = "stun://" + s.account.STUNAddress
		}
		if err := s.helper.Send(map[string]any{"op": "offer", "sdp": sdp, "stun_server": stun}); err != nil {
			return err
		}
		return s.status("connecting", "Negotiating desktop video with your phone.")
	case "BYE", "CANCEL":
		if s.invite == nil || request.Header("Call-ID") != s.invite.Header("Call-ID") {
			return respond(481, "Call/Transaction Does Not Exist")
		}
		if err := respond(200, "OK"); err != nil {
			return err
		}
		if request.Method() == "CANCEL" && s.answer == "" {
			if err := s.sip.SendResponse(s.ctx, *s.invite, 487, "Request Terminated", nil, nil); err != nil {
				return err
			}
		}
		return s.endSession(true)
	case "MESSAGE":
		if s.invite == nil || request.Header("Call-ID") != s.invite.Header("Call-ID") {
			return respond(481, "Call/Transaction Does Not Exist")
		}
		// Desktop input travels only through the helper's validated data channel.
		return respond(200, "OK")
	default:
		return respond(501, "Not Implemented")
	}
}

func (s *remotePCSession) sendAnswer(request remotepc.SIPMessage, answer string) error {
	s.trace("sip answer: sending 200 with encrypted fingerprints")
	return s.sip.SendResponse(s.ctx, request, 200, "OK", []remotepc.SIPHeader{{Name: "Content-Type", Value: "application/sdp"}, {Name: "X-VIVO-PC-Control", Value: "true"}}, []byte(answer))
}

func (s *remotePCSession) handleHost(event remoteHostEvent) error {
	if event.Event == "flow" {
		if event.CaptureFrames >= 0 && event.CaptureFrames <= 1e12 && event.RTPPackets >= 0 && event.RTPPackets <= 1e12 {
			s.trace(fmt.Sprintf("desktop flow: captured=%d rtp=%d", event.CaptureFrames, event.RTPPackets))
		}
		return nil
	}
	if event.Event == "diagnostic" {
		// Log only fixed, non-content milestones; never accept arbitrary helper text.
		switch event.Stage {
		case "capture-frame", "h264-sps", "h264-pps", "h264-idr", "rtp-packet", "channel-open", "heartbeat-request", "heartbeat-reply", "input-message", "input-received", "key-message", "key-has-text", "key-has-modifiers", "key-escape", "key-super", "mouse-message", "mouse-extra-fields", "mouse-missing-position", "mouse-missing-view", "mouse-missing-button", "mouse-extra-position", "mouse-extra-view", "mouse-extra-button", "mouse-rejected", "mouse-received", "mouse-queued", "mouse-outside", "input-queued", "input-unavailable", "video-keyframe-request", "input-move-failed", "input-button-failed", "input-key-failed", "input-text-failed", "input-backend-failed":
			s.trace("desktop " + event.Stage)
		}
		return nil
	}
	switch event.Event {
	case "ready", "answer", "connected", "disconnected", "error":
		s.trace("desktop " + event.Event)
	}
	switch event.Event {
	case "ready":
		s.captureReady = true
		s.control = event.Control
		s.screen = event
		if s.invite != nil && s.screenMessage == nil {
			if err := s.sendScreenInfo(time.Now()); err != nil {
				return err
			}
		}
		return s.reportReady()
	case "answer":
		if s.invite == nil {
			return nil
		}
		if !s.ready() || s.crypto == nil {
			return errors.New("unexpected desktop negotiation response")
		}
		answer, err := s.crypto.EncryptSDP(event.SDP)
		if err != nil {
			return errors.New("cannot secure the desktop video answer")
		}
		if err := s.sendAnswer(*s.invite, answer); err != nil {
			return err
		}
		s.answer = answer
	case "connected":
		if s.invite == nil {
			return nil
		}
		if s.answer == "" || !s.ready() {
			return errors.New("desktop connected without an authorized phone session")
		}
		s.connected = true
		s.sessionDeadline = time.Time{}
		message := "Phone connected. Screen sharing and input control are enabled."
		if !s.control {
			message = "Phone connected. Screen sharing is enabled; input control is unavailable for this desktop session."
		}
		return s.emit(remotePCOutput{Event: "connected", Message: message})
	case "disconnected":
		return s.endSession(true)
	case "error":
		if event.Error == "Desktop ICE gathering timed out." {
			s.trace("desktop ICE gathering timed out")
			return errors.New("desktop ICE candidate gathering timed out")
		}
		return errors.New("desktop capture, input permission, or video negotiation failed")
	}
	return nil
}

func runRemotePC(ctx context.Context, args []string) error {
	options, err := parseRemotePCOptions(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	output := json.NewEncoder(os.Stdout)
	err = runRemotePCSession(ctx, options, os.Stdin, output, remotePCDependencies{
		startHelper: func(path string) (remoteDesktop, error) { return startRemoteHelper(path) },
		bootstrap:   bootstrapRemotePC,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = output.Encode(remotePCOutput{Event: "error", Message: err.Error(), Error: err.Error()})
		return err
	}
	return nil
}

func runRemotePCSession(ctx context.Context, options remotePCOptions, input io.Reader, output *json.Encoder, deps remotePCDependencies) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	id, stored, lock, err := openRemoteState(options.stateDir, options.country)
	if err != nil {
		return err
	}
	defer lock.Close()
	diagnostic, err := os.OpenFile(filepath.Join(options.stateDir, "connection.log"), os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("cannot open private Remote PC diagnostics")
	}
	defer diagnostic.Close()
	if info, err := diagnostic.Stat(); err != nil || !remotePrivate(info, false) {
		return errors.New("Remote PC diagnostics must be a private regular file")
	}
	if diagnostic.Truncate(0) != nil {
		return errors.New("cannot reset Remote PC diagnostics")
	}
	helper, err := deps.startHelper(options.helper)
	if err != nil {
		return err
	}
	defer helper.Close()
	cloud, err := remotepc.NewClient(nil)
	if err != nil {
		return err
	}
	s := &remotePCSession{ctx: ctx, output: output, helper: helper, cloud: cloud, diagnostic: diagnostic}
	if err := s.status("starting", "Choose a screen to share with your phone."); err != nil {
		return err
	}
	if err := helper.Send(map[string]any{"op": "start", "allow_control": true}); err != nil {
		return err
	}
	events := make(chan remotePCEvent, 32)
	post := func(event remotePCEvent) bool {
		if ctx.Err() != nil {
			return false
		}
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	bootstrapStarted := false
	operations := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 1024), 4096)
		for scanner.Scan() {
			var operation struct {
				Op string `json:"op"`
			}
			if json.Unmarshal(scanner.Bytes(), &operation) != nil {
				operation.Op = "invalid"
			}
			select {
			case operations <- operation.Op:
			case <-ctx.Done():
				return
			}
		}
		cancel() // Closing the owning UI's input always releases screen sharing.
	}()
	defer func() {
		cancel()
		if s.signal != nil {
			s.signal.Close()
		}
		if s.sip != nil {
			_ = s.sip.Close()
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var sipDone <-chan struct{}
	for {
		select {
		case <-ctx.Done():
			_ = s.emit(remotePCOutput{Event: "disconnected", Message: "Remote PC stopped."})
			return nil
		case <-sipDone:
			return errors.New("SIP server connection ended; restart Remote PC to reconnect")
		case event, ok := <-helper.Events():
			if !ok {
				return errors.New("Remote PC desktop helper stopped")
			}
			if err := s.handleHost(event); err != nil {
				return err
			}
			if event.Event == "ready" && !bootstrapStarted {
				bootstrapStarted = true
				go deps.bootstrap(ctx, options, id, stored, cloud, post)
			}
		case op := <-operations:
			switch op {
			case "stop":
				cancel()
			case "rotate_password":
				if !s.ready() || s.signal == nil {
					if err := s.emit(remotePCOutput{Event: "error", Message: "Remote PC is not ready to update its password."}); err != nil {
						return err
					}
					continue
				}
				if err := s.endSession(true); err != nil {
					return err
				}
				s.rotating = true
				s.readySent = false
				if err := s.status("connecting", "Updating the temporary password."); err != nil {
					return err
				}
				region, device := s.region, s.device
				go func() {
					password, err := cloud.RefreshPassword(ctx, region, device, options.country)
					post(remotePCEvent{stage: "password", password: password, err: err})
				}()
			default:
				if err := s.emit(remotePCOutput{Event: "error", Message: "Unsupported Remote PC request."}); err != nil {
					return err
				}
			}
		case event := <-events:
			if event.stage == "password" {
				s.rotating = false
				if event.err != nil {
					if err := s.emit(remotePCOutput{Event: "error", Message: "Cannot update the temporary password; try again."}); err != nil {
						return err
					}
				} else {
					s.device.TempPassword = event.password
					if err := s.emit(remotePCOutput{Event: "credentials", DeviceCode: s.device.DeviceNo, TemporaryPassword: s.device.TempPassword}); err != nil {
						return err
					}
				}
				if err := s.reportReady(); err != nil {
					return err
				}
				continue
			}
			if event.err != nil {
				s.trace("failure: " + event.err.Error())
				return event.err
			}
			if event.device != nil {
				s.device = *event.device
				if err := s.emit(remotePCOutput{Event: "credentials", DeviceCode: s.device.DeviceNo, TemporaryPassword: s.device.TempPassword}); err != nil {
					return err
				}
			}
			if event.backend != nil {
				s.region, s.account, s.sip, s.signal = event.backend.region, event.backend.account, event.backend.sip, event.backend.signal
				s.sipReady = true
				sipDone = s.sip.Done()
				go func() {
					for {
						message, err := s.signal.Read(ctx)
						if err != nil {
							post(remotePCEvent{err: fmt.Errorf("Office Kit signaling: %w", err)})
							return
						}
						if !post(remotePCEvent{signal: &message}) {
							return
						}
					}
				}()
			}
			if event.stage != "" && event.backend == nil && event.device == nil {
				if err := s.status("connecting", event.stage); err != nil {
					return err
				}
			}
			if event.sip != nil {
				if s.sip == nil {
					continue
				}
				if err := s.handleSIP(*event.sip); err != nil {
					return err
				}
			}
			if event.signal != nil {
				message := *event.signal
				switch message.Action {
				case "tokenValidation":
					s.signalReady = true
					data := json.RawMessage(`{"type":"sipStatus","sipStatus":1}`)
					if err := s.signal.Send(ctx, remotepc.SignalMessage{Action: "sipStatus", Data: data}); err != nil {
						return err
					}
				case "open":
					if err := s.open(message); err != nil {
						return err
					}
				case "confirm":
					s.confirmScreenInfo(message)
				case "close":
					if message.ConnectionID == s.connection && (message.SourceDeviceNo == "" || message.SourceDeviceNo == s.peer) {
						if err := s.endSession(false); err != nil {
							return err
						}
					}
				case "error":
					return errors.New("Office Kit rejected the remote session; restart Remote PC to reconnect")
				case "sipInfoChange":
					return errors.New("Office Kit changed its SIP server; restart Remote PC to reconnect")
				}
			}
			if err := s.reportReady(); err != nil {
				return err
			}
		case now := <-ticker.C:
			if err := s.retryScreenInfo(now); err != nil {
				return err
			}
			if !s.sessionDeadline.IsZero() && time.Now().After(s.sessionDeadline) {
				if err := s.endSession(true); err != nil {
					return err
				}
				if err := s.status("connecting", "The phone connection timed out. Preparing screen sharing again."); err != nil {
					return err
				}
			}
		}
	}
}

func bootstrapRemotePC(ctx context.Context, options remotePCOptions, id vivo.Identity, stored remoteStoredState, cloud *remotepc.Client, post func(remotePCEvent) bool) {
	fail := func(message string) { post(remotePCEvent{err: errors.New(message)}) }
	post(remotePCEvent{stage: "Finding the regional Office Kit server."})
	// The shipped backend defines Windows=1 and macOS=3, with no Linux enum.
	// This explicit interoperability experiment uses 1 with a fresh native identity.
	region, err := cloud.ResolveRegion(ctx, options.country, 1)
	if err != nil {
		fail("Cannot reach the regional Office Kit server.")
		return
	}
	post(remotePCEvent{stage: "Registering this native Linux desktop."})
	identity := sha256.Sum256([]byte(id.DeviceID))
	device, err := cloud.Register(ctx, region, remotepc.Registration{DeviceType: 1, Identity: hex.EncodeToString(identity[:]), DeviceDefaultName: "VOkit Linux", Version: "3.0.4", PCDeviceID: id.DeviceID, DeviceToken: stored.DeviceToken, PriDeviceToken: stored.PriDeviceToken, CountryCode: options.country})
	if err != nil {
		fail("Office Kit desktop registration failed.")
		return
	}
	if device.PriDeviceToken == "" {
		device.PriDeviceToken = stored.PriDeviceToken
		if device.PriDeviceToken == "" {
			device.PriDeviceToken = device.DeviceToken
		}
	}
	if err := saveRemoteState(options.stateDir, options.country, device); err != nil {
		fail(err.Error())
		return
	}
	if !post(remotePCEvent{device: &device}) {
		return
	}
	post(remotePCEvent{stage: "Retrieving encrypted SIP credentials."})
	account, err := cloud.GetSIPAccount(ctx, region, device)
	if err != nil {
		fail("Office Kit SIP credential retrieval failed.")
		return
	}
	post(remotePCEvent{stage: "Connecting securely to the SIP server."})
	sip, err := remotepc.DialSIP(ctx, account, remotepc.SIPConfig{OnRequest: func(_ *remotepc.SIPClient, request remotepc.SIPMessage) { post(remotePCEvent{sip: &request}) }})
	if err != nil {
		fail("SIP server TLS connection failed. The server may be unavailable; try again later.")
		return
	}
	go func() { <-ctx.Done(); _ = sip.Close() }()
	owned := true
	defer func() {
		if owned {
			_ = sip.Close()
		}
	}()
	if err := sip.Register(ctx); err != nil {
		fail("SIP server registration failed.")
		return
	}
	post(remotePCEvent{stage: "Authenticating Office Kit signaling."})
	signal, err := remotepc.DialSignaling(ctx, region, device)
	if err != nil {
		fail("Office Kit signaling TLS connection failed.")
		return
	}
	if !post(remotePCEvent{backend: &remoteBackend{region, account, sip, signal}}) {
		signal.Close()
		return
	}
	owned = false
}
