package remotepc

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const maxSignalBytes = 1024 * 1024

type SignalAbility struct {
	PasswordType int `json:"passwordType,omitempty"`
	DCHeartBeat  int `json:"dcHeartBeat,omitempty"`
}

type SignalMessage struct {
	MsgID            string          `json:"msgId"`
	TimeStamp        int64           `json:"timeStamp"`
	Action           string          `json:"action"`
	SourceDeviceNo   string          `json:"sourceDeviceNo"`
	TargetDeviceNo   string          `json:"targetDeviceNo"`
	Data             json.RawMessage `json:"data"`
	Version          string          `json:"version"`
	ConnectionID     string          `json:"connectionId"`
	ConnectionType   int             `json:"connectionType,omitempty"`
	TargetDeviceName string          `json:"targetDeviceName,omitempty"`
	Ability          SignalAbility   `json:"ability,omitempty"`
}

func (m *SignalMessage) UnmarshalJSON(raw []byte) error {
	type fields SignalMessage
	var value fields
	wire := struct {
		*fields
		TimeStamp json.RawMessage `json:"timeStamp"`
	}{fields: &value}
	if err := json.Unmarshal(raw, &wire); err != nil {
		var field *json.UnmarshalTypeError
		if errors.As(err, &field) {
			return fmt.Errorf("invalid remote signaling field %s (expected %s)", field.Field, field.Type)
		}
		return errors.New("invalid remote signaling fields")
	}
	// The production gateway quotes timestamps; peers may send JSON numbers.
	if len(wire.TimeStamp) != 0 && string(wire.TimeStamp) != "null" {
		var err error
		if wire.TimeStamp[0] == '"' {
			var text string
			if json.Unmarshal(wire.TimeStamp, &text) != nil {
				return errors.New("invalid remote signaling timestamp")
			}
			value.TimeStamp, err = strconv.ParseInt(text, 10, 64)
		} else {
			err = json.Unmarshal(wire.TimeStamp, &value.TimeStamp)
		}
		if err != nil || value.TimeStamp < 0 {
			return errors.New("invalid remote signaling timestamp")
		}
	}
	*m = SignalMessage(value)
	return nil
}

// Signaling maintains the authenticated cloud socket and its JSON heartbeat.
// Reading an open request never starts capture, accepts SIP, or executes input.
type Signaling struct {
	conn     *websocket.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	deviceNo string
	mu       sync.Mutex
	writeMu  sync.Mutex
	authed   bool
	peer     string
	session  string
}

func DialSignaling(ctx context.Context, region Region, device Device) (*Signaling, error) {
	return dialSignaling(ctx, region, device, nil)
}

func dialSignaling(ctx context.Context, region Region, device Device, client *http.Client) (*Signaling, error) {
	if !validBase(region.WebsocketDomain, "wss") || !signalIdentifier(device.DeviceNo) || device.DeviceToken == "" || !validValue(device.DeviceToken, 8192) {
		return nil, errors.New("invalid remote signaling configuration")
	}
	u, _ := url.Parse(region.WebsocketDomain)
	u.Path = "/ws/v1/servicename/remote-desktop/id/" + device.DeviceNo
	u.RawQuery = url.Values{
		"controlType": {"0"}, "retry": {"0"}, "pcProcessId": {strconv.Itoa(os.Getpid())},
		"fileTransport": {"0"}, "needAccountAuth": {"0"}, "p2pSecurity": {"1"},
	}.Encode()
	if client == nil {
		client = http.DefaultClient
	}
	httpClient := *client
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dialCtx, stop := context.WithTimeout(ctx, 10*time.Second)
	conn, response, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{
		HTTPClient: &httpClient,
		HTTPHeader: http.Header{
			"Origin": {"https://wwww.vivo.xyz"}, "Openid": {""}, "Devicetoken": {device.DeviceToken},
		},
	})
	stop()
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("remote signaling WebSocket TLS/upgrade failed")
	}
	conn.SetReadLimit(maxSignalBytes)
	lifetime, cancel := context.WithCancel(ctx)
	s := &Signaling{conn: conn, ctx: lifetime, cancel: cancel, done: make(chan struct{}), deviceNo: device.DeviceNo}
	go s.heartbeat()
	return s, nil
}

func (s *Signaling) heartbeat() {
	defer close(s.done)
	defer s.conn.CloseNow()
	ticker := time.NewTicker(4 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.Send(s.ctx, SignalMessage{Action: "ping"}) != nil {
				s.cancel()
				return
			}
		}
	}
}

func (s *Signaling) Close() {
	s.cancel()
	s.conn.CloseNow()
	<-s.done
}

// Read has one reader, matching coder/websocket's connection contract.
func (s *Signaling) Read(ctx context.Context) (SignalMessage, error) {
	kind, raw, err := s.conn.Read(ctx)
	if err != nil {
		return SignalMessage{}, s.readError(ctx)
	}
	var message SignalMessage
	if kind != websocket.MessageText {
		s.cancel()
		return SignalMessage{}, errors.New("unexpected binary remote signaling message")
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		s.cancel()
		return SignalMessage{}, fmt.Errorf("cannot decode remote signaling: %w", err)
	}
	if message.Action == "" || !validValue(message.Action, 64) || (message.Version != "" && message.Version != "v1") {
		s.cancel()
		return SignalMessage{}, errors.New("invalid remote signaling message")
	}
	// Gateway pongs echo the ping's peer routing. They cannot authorize a session.
	if message.Action == "pong" {
		return message, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if message.TargetDeviceNo != "" && message.TargetDeviceNo != s.deviceNo {
		s.cancel()
		switch message.Action {
		case "confirm", "sipStatus", "pong", "openSuccess":
			return SignalMessage{}, fmt.Errorf("remote signaling target does not match this device for %s", message.Action)
		default:
			return SignalMessage{}, errors.New("remote signaling target does not match this device")
		}
	}
	if message.Action == "tokenValidation" {
		s.authed = true
	}
	if message.Action == "open" {
		var peer string
		if !s.authed || !signalIdentifier(message.ConnectionID) || json.Unmarshal(message.Data, &peer) != nil || !signalIdentifier(peer) || peer == s.deviceNo || (message.SourceDeviceNo != "" && message.SourceDeviceNo != peer) || message.ConnectionType != 0 || (s.session != "" && (s.session != message.ConnectionID || s.peer != peer)) {
			s.cancel()
			return SignalMessage{}, errors.New("invalid or unsupported remote open request")
		}
		s.peer, s.session = peer, message.ConnectionID
	}
	if message.Action == "close" {
		// SIP BYE can clear the session before its delayed cloud close arrives.
		if s.session == "" || message.ConnectionID != s.session {
			return message, nil
		}
		if message.SourceDeviceNo != "" && message.SourceDeviceNo != s.peer {
			return SignalMessage{}, errors.New("remote close does not match the active session")
		}
		s.peer, s.session = "", ""
	}
	return message, nil
}

func (s *Signaling) readError(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	return errors.New("remote signaling connection closed or read failed")
}

func (s *Signaling) Send(ctx context.Context, message SignalMessage) error {
	if message.Action == "" || !validValue(message.Action, 64) || (message.SourceDeviceNo != "" && message.SourceDeviceNo != s.deviceNo) {
		return errors.New("invalid remote signaling envelope")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	if message.TargetDeviceNo == "" {
		message.TargetDeviceNo = s.peer
	}
	if message.ConnectionID == "" {
		message.ConnectionID = s.session
	}
	s.mu.Unlock()
	return s.writeSignal(ctx, message)
}

// EndSession releases the cloud relationship and clears local routing even if
// notification fails. Serialize with heartbeats so none retain the old peer.
func (s *Signaling) EndSession(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == "" {
		return nil
	}
	message := SignalMessage{
		Action: "closeConnection", TargetDeviceNo: s.peer, ConnectionID: s.session,
		Data: json.RawMessage(`{"type":"closeConnection"}`),
	}
	defer func() { s.peer, s.session = "", "" }()
	return s.writeSignal(ctx, message)
}

func (s *Signaling) writeSignal(ctx context.Context, message SignalMessage) error {
	message.SourceDeviceNo = s.deviceNo
	message.Version = "v1"
	if message.MsgID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return errors.New("cannot generate signaling message ID")
		}
		id[6], id[8] = (id[6]&15)|64, (id[8]&63)|128
		message.MsgID = fmt.Sprintf("%x-%x-%x-%x-%x%d", id[:4], id[4:6], id[6:8], id[8:10], id[10:], time.Now().UnixMilli())
	}
	if message.TimeStamp == 0 {
		message.TimeStamp = time.Now().UnixMilli()
	}
	if message.Data == nil {
		message.Data = json.RawMessage(`""`)
	}
	raw, err := json.Marshal(map[string]any{
		"msgId": message.MsgID, "timeStamp": message.TimeStamp, "action": message.Action,
		"sourceDeviceNo": message.SourceDeviceNo, "targetDeviceNo": message.TargetDeviceNo,
		"data": message.Data, "version": message.Version, "connectionId": message.ConnectionID,
	})
	if err != nil || len(raw) > maxSignalBytes {
		return errors.New("invalid or oversized remote signaling payload")
	}
	writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err := s.conn.Write(writeCtx, websocket.MessageText, raw); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("remote signaling write failed")
	}
	return nil
}

func signalIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-", c) {
			return false
		}
	}
	return true
}
