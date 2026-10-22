package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vivolinux/internal/remotepc"
	"vivolinux/internal/vivo"
)

type fakeRemoteHelper struct {
	sent   []any
	events chan remoteHostEvent
	closed chan struct{}
}

func (h *fakeRemoteHelper) Send(value any) error           { h.sent = append(h.sent, value); return nil }
func (h *fakeRemoteHelper) Events() <-chan remoteHostEvent { return h.events }
func (h *fakeRemoteHelper) Close() {
	if h.closed != nil {
		close(h.closed)
	}
}

type fakeRemoteSignal struct {
	sent  []remotepc.SignalMessage
	ended int
}

func (s *fakeRemoteSignal) Read(context.Context) (remotepc.SignalMessage, error) {
	return remotepc.SignalMessage{}, io.EOF
}
func (s *fakeRemoteSignal) Send(_ context.Context, message remotepc.SignalMessage) error {
	s.sent = append(s.sent, message)
	return nil
}
func (s *fakeRemoteSignal) EndSession(context.Context) error { s.ended++; return nil }
func (s *fakeRemoteSignal) Close()                           {}

type fakeRemoteResponse struct {
	request remotepc.SIPMessage
	status  int
	body    []byte
	headers []remotepc.SIPHeader
}
type fakeRemoteSIP struct{ responses []fakeRemoteResponse }

func (s *fakeRemoteSIP) SendResponse(_ context.Context, request remotepc.SIPMessage, status int, _ string, headers []remotepc.SIPHeader, body []byte) error {
	s.responses = append(s.responses, fakeRemoteResponse{request, status, body, headers})
	return nil
}
func (s *fakeRemoteSIP) Done() <-chan struct{} { return nil }
func (s *fakeRemoteSIP) Err() error            { return nil }
func (s *fakeRemoteSIP) Close() error          { return nil }

func testRemoteSession() (*remotePCSession, *fakeRemoteHelper, *fakeRemoteSignal, *fakeRemoteSIP, *bytes.Buffer) {
	h := &fakeRemoteHelper{events: make(chan remoteHostEvent, 8)}
	signal := &fakeRemoteSignal{}
	sip := &fakeRemoteSIP{}
	out := &bytes.Buffer{}
	s := &remotePCSession{ctx: context.Background(), output: json.NewEncoder(out), helper: h, signal: signal, sip: sip, device: remotepc.Device{DeviceNo: "1234567890", TempPassword: "synthetic-password"}, account: remotepc.SIPAccount{SIPUserName: "pc-sip-user", STUNAddress: "stun.example.test:3478"}}
	return s, h, signal, sip, out
}
func remoteOpenMessage() remotepc.SignalMessage {
	return remotepc.SignalMessage{Action: "open", Data: json.RawMessage(`"phone-peer"`), ConnectionID: "synthetic-session", Ability: remotepc.SignalAbility{PasswordType: 1}}
}
func remoteInvite(peer string) remotepc.SIPMessage {
	return remotepc.SIPMessage{StartLine: "INVITE sip:pc-sip-user@logical.example.test SIP/2.0", Headers: []remotepc.SIPHeader{{Name: "From", Value: "<sip:" + peer + "@logical.example.test>;tag=peer"}, {Name: "To", Value: "<sip:pc-sip-user@logical.example.test>"}, {Name: "Call-ID", Value: "synthetic-call"}, {Name: "CSeq", Value: "1 INVITE"}, {Name: "Content-Type", Value: "application/sdp"}}}
}

func TestRemotePCDiagnosticsDoNotRecordCredentials(t *testing.T) {
	s, _, _, _, _ := testRemoteSession()
	var trace bytes.Buffer
	s.diagnostic = &trace
	s.captureReady, s.sipReady, s.signalReady = true, true, true
	if err := s.open(remoteOpenMessage()); err != nil {
		t.Fatal(err)
	}
	if err := s.handleSIP(remoteInvite("unauthorized-secret-peer")); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{s.device.TempPassword, s.device.DeviceNo, "unauthorized-secret-peer", "phone-peer", "synthetic-session"} {
		if strings.Contains(trace.String(), secret) {
			t.Fatal("credential or peer identity written to diagnostics")
		}
	}
	if !strings.Contains(trace.String(), "sip INVITE: authorized=false") {
		t.Fatal("missing useful stage diagnostic")
	}
	for i := 0; i < 300; i++ {
		s.trace("bounded")
	}
	if strings.Count(trace.String(), "\n") != 256 {
		t.Fatal("diagnostic record limit was not enforced")
	}
}

func TestRemotePCReadinessAndUnsupportedOpen(t *testing.T) {
	for _, missing := range []string{"capture", "sip", "signaling", "rotating"} {
		t.Run(missing, func(t *testing.T) {
			s, _, signal, _, out := testRemoteSession()
			s.captureReady = true
			s.sipReady = true
			s.signalReady = true
			switch missing {
			case "capture":
				s.captureReady = false
			case "sip":
				s.sipReady = false
			case "signaling":
				s.signalReady = false
			case "rotating":
				s.rotating = true
			}
			if err := s.reportReady(); err != nil {
				t.Fatal(err)
			}
			if out.Len() != 0 {
				t.Fatal("advertised readiness before every prerequisite")
			}
			if err := s.open(remoteOpenMessage()); err != nil {
				t.Fatal(err)
			}
			if len(signal.sent) != 0 || signal.ended != 1 || s.peer != "" {
				t.Fatal("accepted a phone before readiness")
			}
		})
	}
	for _, passwordType := range []int{0, 2} {
		s, _, signal, _, _ := testRemoteSession()
		s.captureReady = true
		s.sipReady = true
		s.signalReady = true
		message := remoteOpenMessage()
		message.Ability.PasswordType = passwordType
		if err := s.open(message); err != nil {
			t.Fatal(err)
		}
		if len(signal.sent) != 0 || s.crypto != nil {
			t.Fatal("accepted unsupported password mode")
		}
	}
	s, _, _, _, out := testRemoteSession()
	s.captureReady = true
	s.sipReady = true
	s.signalReady = true
	if err := s.reportReady(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Input control is unavailable") {
		t.Fatal("view-only readiness was not explained")
	}
}

func TestRemotePCGuardsSIPAndNegotiatesAuthorizedSDP(t *testing.T) {
	s, helper, signal, sip, out := testRemoteSession()
	s.captureReady = true
	s.sipReady = true
	s.signalReady = true
	s.control = true
	request := remoteInvite("phone-peer")
	if err := s.handleSIP(request); err != nil {
		t.Fatal(err)
	}
	if sip.responses[len(sip.responses)-1].status != 403 || len(helper.sent) != 0 {
		t.Fatal("accepted an INVITE without authenticated cloud open")
	}
	if err := s.open(remoteOpenMessage()); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != 1 || signal.sent[0].Action != "openSuccess" || s.crypto == nil {
		t.Fatal("missing protected open response")
	}
	if err := s.handleSIP(remoteInvite("different-phone")); err != nil {
		t.Fatal(err)
	}
	if sip.responses[len(sip.responses)-1].status != 403 || len(helper.sent) != 0 {
		t.Fatal("accepted mismatched SIP peer")
	}
	if err := s.handleSIP(request); err != nil {
		t.Fatal(err)
	}
	if sip.responses[len(sip.responses)-1].status != 488 {
		t.Fatal("accepted SDP without authenticated fingerprints")
	}
	fingerprint := strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	plain := "v=0\r\na=fingerprint:sha-256 " + fingerprint + "\r\na=rtpmap:96 H264/90000\r\n"
	encrypted, err := s.crypto.EncryptSDP(plain)
	if err != nil {
		t.Fatal(err)
	}
	request.Body = []byte(encrypted)
	if err := s.handleSIP(request); err != nil {
		t.Fatal(err)
	}
	if len(helper.sent) != 1 {
		t.Fatal("authorized offer not handed to capture helper")
	}
	offer := helper.sent[0].(map[string]any)
	if offer["op"] != "offer" || offer["sdp"] != plain || offer["stun_server"] != "stun://stun.example.test:3478" {
		t.Fatal("incorrect native media offer")
	}
	if err := s.handleHost(remoteHostEvent{Event: "answer", SDP: plain}); err != nil {
		t.Fatal(err)
	}
	response := sip.responses[len(sip.responses)-1]
	decoded, err := s.crypto.DecryptSDP(string(response.body))
	if response.status != 200 || err != nil || decoded != plain {
		t.Fatal("SIP answer did not carry authenticated fingerprints")
	}
	if err := s.handleSIP(request); err != nil {
		t.Fatal(err)
	}
	if len(helper.sent) != 1 || string(sip.responses[len(sip.responses)-1].body) != string(response.body) {
		t.Fatal("retransmitted INVITE changed negotiation")
	}
	if err := s.handleHost(remoteHostEvent{Event: "connected"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"event":"connected"`) {
		t.Fatal("missing connected event")
	}
	bye := request
	bye.StartLine = "BYE sip:pc-sip-user@logical.example.test SIP/2.0"
	if err := s.handleSIP(bye); err != nil {
		t.Fatal(err)
	}
	if s.peer != "" || s.crypto != nil || s.captureReady || s.readySent || signal.ended != 1 || len(helper.sent) != 2 {
		t.Fatal("disconnect retained access or capture readiness")
	}
	if err := s.handleSIP(request); err != nil {
		t.Fatal(err)
	}
	if sip.responses[len(sip.responses)-1].status != 403 {
		t.Fatal("old authorized peer survived disconnect")
	}
}

func TestRemotePCPrivateStableState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	id, _, lock, err := openRemoteState(dir, "IN")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, other, err := openRemoteState(dir, "IN"); err == nil {
		other.Close()
		t.Fatal("allowed duplicate native host")
	}
	device := remotepc.Device{DeviceNo: "1234567890", DeviceToken: "synthetic-token", PriDeviceToken: "synthetic-prior", TempPassword: "never-store-this-password"}
	if err := saveRemoteState(dir, "IN", device); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	data, err := os.ReadFile(filepath.Join(dir, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(device.TempPassword)) {
		t.Fatal("persisted temporary password")
	}
	second, stored, lock, err := openRemoteState(dir, "IN")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if id.DeviceID != second.DeviceID || stored.DeviceToken != device.DeviceToken {
		t.Fatal("native identity was not stable")
	}
	if _, _, lock, err := openRemoteState(dir, "SG"); err == nil {
		lock.Close()
		t.Fatal("reused device credentials across countries")
	}
	if err := os.Chmod(filepath.Join(dir, "device.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, lock, err := openRemoteState(dir, "IN"); err == nil {
		lock.Close()
		t.Fatal("accepted public credential state")
	}
}

func TestRemotePCLifecycleStopsOnEOFWithoutCloud(t *testing.T) {
	inputWriter, inputReader := func() (*io.PipeWriter, *io.PipeReader) { r, w := io.Pipe(); return w, r }()
	outReader, outWriter := io.Pipe()
	defer outReader.Close()
	helper := &fakeRemoteHelper{events: make(chan remoteHostEvent, 2), closed: make(chan struct{})}
	bootstrapStarted := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runRemotePCSession(ctx, remotePCOptions{stateDir: filepath.Join(t.TempDir(), "state"), helper: "synthetic-helper", country: "IN"}, inputReader, json.NewEncoder(outWriter), remotePCDependencies{
			startHelper: func(string) (remoteDesktop, error) { return helper, nil },
			bootstrap: func(ctx context.Context, _ remotePCOptions, _ vivo.Identity, _ remoteStoredState, _ *remotepc.Client, _ func(remotePCEvent) bool) {
				bootstrapStarted <- struct{}{}
				<-ctx.Done()
			},
		})
		outWriter.Close()
	}()
	decoder := json.NewDecoder(outReader)
	var event remotePCOutput
	if err := decoder.Decode(&event); err != nil || event.State != "starting" {
		t.Fatalf("missing startup event: %v", err)
	}
	select {
	case <-bootstrapStarted:
		t.Fatal("contacted cloud before screen-sharing permission")
	case <-time.After(25 * time.Millisecond):
	}
	helper.events <- remoteHostEvent{Event: "ready", Control: true}
	select {
	case <-bootstrapStarted:
	case <-ctx.Done():
		t.Fatal("did not start cloud connection after screen sharing became ready")
	}
	if _, err := io.WriteString(inputWriter, "{\"op\":\"unsupported\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&event); err != nil || event.Event != "error" || event.Error == "" {
		t.Fatalf("readiness or input error contract broken: %v", err)
	}
	inputWriter.Close()
	if err := decoder.Decode(&event); err != nil || event.Event != "disconnected" {
		t.Fatalf("EOF did not stop remote access: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-helper.closed:
	default:
		t.Fatal("EOF did not release helper")
	}
}

func TestRemotePCRejectsInvalidOptions(t *testing.T) {
	for _, args := range [][]string{{"--country", "in"}, {"--country", ""}, {"--state-dir", ""}, {"unexpected"}} {
		if _, err := parseRemotePCOptions(args); err == nil {
			t.Fatalf("accepted invalid options %v", args)
		}
	}
}

func TestRemotePCDesktopStartupMetadata(t *testing.T) {
	s, _, signal, _, _ := testRemoteSession()
	s.sipReady, s.signalReady = true, true
	unlocked := false
	if err := s.handleHost(remoteHostEvent{Event: "ready", CaptureWidth: 1920, CaptureHeight: 1080, ScreenX: 100, ScreenY: 200, ScreenScale: 1.25, ScreenLocked: &unlocked}); err != nil {
		t.Fatal(err)
	}
	if err := s.open(remoteOpenMessage()); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != 2 || signal.sent[0].Action != "msg" || signal.sent[1].Action != "openSuccess" {
		t.Fatal("screen lock state must precede protected open response")
	}
	var lock struct {
		Type   string
		IsLock bool
	}
	if err := json.Unmarshal(signal.sent[0].Data, &lock); err != nil || lock.Type != "screenLockInfo" || lock.IsLock {
		t.Fatal("incorrect explicit unlocked state")
	}
	// No screen metadata may leave for a mismatched or unauthenticated INVITE.
	if err := s.handleSIP(remoteInvite("wrong-peer")); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != 2 {
		t.Fatal("metadata disclosed to unauthorized peer")
	}
	request := remoteInvite("phone-peer")
	fingerprint := strings.TrimSuffix(strings.Repeat("AB:", 32), ":")
	encrypted, err := s.crypto.EncryptSDP("v=0\r\na=fingerprint:sha-256 " + fingerprint + "\r\na=rtpmap:98 H264/90000\r\n")
	if err != nil {
		t.Fatal(err)
	}
	request.Body = []byte(encrypted)
	if err := s.handleSIP(request); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != 4 {
		t.Fatalf("missing screen and DPI messages: %d", len(signal.sent))
	}
	screen := signal.sent[2]
	var info struct {
		Type               string
		CurrentScreenIndex int
		MScreenInfos       []struct {
			ID, Index, Width, Height, X, Y int
			Selected                       bool
		}
	}
	if err := json.Unmarshal(screen.Data, &info); err != nil {
		t.Fatal(err)
	}
	if info.Type != "deviceScreenInfo" || info.CurrentScreenIndex != 0 || len(info.MScreenInfos) != 1 {
		t.Fatalf("wrong selected-display metadata: %s", screen.Data)
	}
	d := info.MScreenInfos[0]
	if d.ID != 0 || d.Index != 0 || !d.Selected || d.Width != 1920 || d.Height != 1080 || d.X != 100 || d.Y != 200 {
		t.Fatalf("metadata changed source geometry: %s", screen.Data)
	}
	var dpi struct {
		Type  string
		Value float64
	}
	if json.Unmarshal(signal.sent[3].Data, &dpi) != nil || dpi.Type != "deviceScreenDpiInfo" || dpi.Value != 1.25 {
		t.Fatal("wrong fractional DPI")
	}
	if screen.MsgID == "" || screen.ConnectionID != s.connection || screen.TargetDeviceNo != s.peer {
		t.Fatal("screen information lacked authenticated routing")
	}
	// SIP retransmission does not restart metadata retries or create new ids.
	if err := s.handleSIP(request); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != 4 {
		t.Fatal("SIP retransmit repeated startup metadata")
	}
}

func TestRemotePCScreenMetadataRetriesAndConfirmation(t *testing.T) {
	s, _, signal, _, _ := testRemoteSession()
	s.captureReady, s.sipReady, s.signalReady = true, true, true
	if err := s.open(remoteOpenMessage()); err != nil {
		t.Fatal(err)
	}
	s.invite = &remotepc.SIPMessage{}
	s.screen = remoteHostEvent{CaptureWidth: 2560, CaptureHeight: 1440}
	now := time.Now()
	if err := s.sendScreenInfo(now); err != nil {
		t.Fatal(err)
	}
	original := *s.screenMessage
	confirm := remotepc.SignalMessage{Action: "confirm", MsgID: original.MsgID, ConnectionID: s.connection, SourceDeviceNo: s.peer, TargetDeviceNo: s.device.DeviceNo, Data: json.RawMessage(`{"type":"deviceScreenInfo"}`)}
	for _, field := range []string{"action", "id", "connection", "source", "target", "type"} {
		bad := confirm
		switch field {
		case "action":
			bad.Action = "msg"
		case "id":
			bad.MsgID = "other"
		case "connection":
			bad.ConnectionID = "other"
		case "source":
			bad.SourceDeviceNo = "other"
		case "target":
			bad.TargetDeviceNo = "other"
		case "type":
			bad.Data = json.RawMessage(`{"type":"screenLockInfo"}`)
		}
		s.confirmScreenInfo(bad)
		if s.screenMessage == nil {
			t.Fatalf("accepted mismatched %s confirmation", field)
		}
	}
	before := len(signal.sent)
	if err := s.retryScreenInfo(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != before {
		t.Fatal("retry ignored deadline")
	}
	if err := s.retryScreenInfo(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	retry := signal.sent[len(signal.sent)-1]
	if retry.MsgID != original.MsgID || !bytes.Equal(retry.Data, original.Data) {
		t.Fatal("retry changed screen message identity or payload")
	}
	s.confirmScreenInfo(confirm)
	if s.screenMessage != nil {
		t.Fatal("matching confirmation failed to stop retries")
	}
	before = len(signal.sent)
	if err := s.retryScreenInfo(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != before {
		t.Fatal("retried acknowledged screen info")
	}
	// Three total attempts, then stop without disconnecting or blocking video.
	if err := s.sendScreenInfo(now); err != nil {
		t.Fatal(err)
	}
	before = len(signal.sent)
	for i := 1; i <= 6; i++ {
		if err := s.retryScreenInfo(now.Add(time.Duration(i) * 2 * time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if len(signal.sent) != before+2 || s.screenMessage != nil {
		t.Fatal("screen metadata retries were not bounded to three attempts")
	}
	if err := s.sendScreenInfo(now); err != nil {
		t.Fatal(err)
	}
	if err := s.endSession(false); err != nil {
		t.Fatal(err)
	}
	if s.screenMessage != nil {
		t.Fatal("ended session retained pending metadata")
	}
}

func TestRemotePCUnknownDesktopMetadataNotInvented(t *testing.T) {
	s, _, signal, _, _ := testRemoteSession()
	s.captureReady, s.sipReady, s.signalReady = true, true, true
	if err := s.open(remoteOpenMessage()); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != 1 || signal.sent[0].Action != "openSuccess" {
		t.Fatal("invented an unknown desktop lock state")
	}
	s.invite = &remotepc.SIPMessage{}
	if err := s.sendScreenInfo(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(signal.sent) != 1 {
		t.Fatal("invented unknown source dimensions")
	}
}

func TestRemotePCHelperDiagnosticsWhitelist(t *testing.T) {
	s, _, _, _, out := testRemoteSession()
	var trace bytes.Buffer
	s.diagnostic = &trace
	for _, stage := range []string{"capture-frame", "h264-sps", "h264-pps", "h264-idr", "rtp-packet", "channel-open", "heartbeat-request", "heartbeat-reply", "input-message", "input-received", "key-message", "key-has-text", "key-has-modifiers", "key-escape", "key-super", "mouse-message", "mouse-extra-fields", "mouse-missing-position", "mouse-missing-view", "mouse-missing-button", "mouse-extra-position", "mouse-extra-view", "mouse-extra-button", "mouse-rejected", "mouse-received", "mouse-queued", "mouse-outside", "input-queued", "input-unavailable", "video-keyframe-request", "input-move-failed", "input-button-failed", "input-key-failed", "input-text-failed", "input-backend-failed"} {
		if err := s.handleHost(remoteHostEvent{Event: "diagnostic", Stage: stage}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(trace.String(), "desktop "+stage+"\n") {
			t.Fatalf("missing diagnostic %s", stage)
		}
	}
	before := trace.String()
	for _, stage := range []string{"", "h264-idr\nsecret-password", "input-received private text", "arbitrary-secret"} {
		if err := s.handleHost(remoteHostEvent{Event: "diagnostic", Stage: stage, Error: "private error", SDP: "private SDP"}); err != nil {
			t.Fatal(err)
		}
	}
	if trace.String() != before || out.Len() != 0 || s.captureReady || s.connected {
		t.Fatal("helper diagnostic leaked content or changed session state")
	}
}

func TestRemotePCFlowDiagnosticsContainCountsOnly(t *testing.T) {
	s, _, _, _, out := testRemoteSession()
	var trace bytes.Buffer
	s.diagnostic = &trace
	if err := s.handleHost(remoteHostEvent{Event: "flow", CaptureFrames: 150, RTPPackets: 300, Error: "private contents"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(trace.String(), "desktop flow: captured=150 rtp=300") || strings.Contains(trace.String(), "private") || out.Len() != 0 {
		t.Fatal("invalid flow diagnostic")
	}
	before := trace.String()
	for _, count := range []int64{-1, 1000000000001} {
		if err := s.handleHost(remoteHostEvent{Event: "flow", CaptureFrames: count}); err != nil {
			t.Fatal(err)
		}
	}
	if before != trace.String() {
		t.Fatal("invalid flow counts logged")
	}
}
