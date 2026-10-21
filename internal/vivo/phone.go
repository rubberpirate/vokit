package vivo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type PhoneClient struct {
	base  string
	http  *http.Client
	trust *certificateTrust
}

var errCertificatePinMismatch = errors.New("phone certificate changed; pinned certificate mismatch")

var ErrControlDisconnected = errors.New("phone closed the control transport; scan a fresh QR to reconnect")

type certificateTrust struct {
	mu  sync.Mutex
	pin []byte
}

func (p *PhoneClient) CertificatePin() string {
	if p.trust == nil {
		return ""
	}
	p.trust.mu.Lock()
	defer p.trust.mu.Unlock()
	return hex.EncodeToString(p.trust.pin)
}

// NewFirstUsePhoneClient explicitly trusts the first certificate on the actual
// request connection, then pins it. The caller must authorize LAN first-use trust.
func NewFirstUsePhoneClient(address netip.Addr) (*PhoneClient, error) {
	if address.Zone() != "" || !address.Unmap().IsPrivate() {
		return nil, errors.New("phone must have a private LAN address without an interface zone")
	}
	return newPhoneClientTrust("https://"+net.JoinHostPort(address.Unmap().String(), "10380"), "", true)
}

func NewPhoneClient(address netip.Addr, pin string) (*PhoneClient, error) {
	if address.Zone() != "" {
		return nil, errors.New("phone address must not contain an interface zone")
	}
	address = address.Unmap()
	if !address.IsPrivate() || address.Zone() != "" {
		return nil, errors.New("phone must have a private LAN address")
	}
	return newPhoneClient("https://"+net.JoinHostPort(address.String(), "10380"), pin)
}

func newPhoneClient(base, pin string) (*PhoneClient, error) {
	return newPhoneClientTrust(base, pin, false)
}

func newPhoneClientTrust(base, pin string, firstUse bool) (*PhoneClient, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("invalid phone HTTPS endpoint")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	var trust *certificateTrust
	if pin != "" || firstUse {
		if pin != "" && firstUse {
			return nil, errors.New("choose explicit pin or first-use trust")
		}
		trust = &certificateTrust{}
		if pin != "" {
			fingerprint, err := ParseCertificatePin(pin)
			if err != nil {
				return nil, err
			}
			trust.pin = fingerprint
		}
		// A supplied exact certificate pin replaces CA/hostname trust, never peer verification.
		config.InsecureSkipVerify = true
		config.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("phone certificate missing")
			}
			hash := sha256.Sum256(state.PeerCertificates[0].Raw)
			trust.mu.Lock()
			defer trust.mu.Unlock()
			if len(trust.pin) == 0 && firstUse {
				trust.pin = append([]byte(nil), hash[:]...)
			}
			if subtle.ConstantTimeCompare(hash[:], trust.pin) != 1 {
				return errCertificatePinMismatch
			}
			return nil
		}
	}
	transport := &http.Transport{TLSClientConfig: config, Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
	var roundTripper http.RoundTripper = transport
	if firstUse {
		config.NextProtos = []string{"http/1.1"}
		roundTripper = &phoneConnection{endpoint: u.Host, config: config}
	}
	return &PhoneClient{base: base, trust: trust, http: &http.Client{Transport: roundTripper, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (p *PhoneClient) Close() { p.http.CloseIdleConnections() }

func (p *PhoneClient) post(ctx context.Context, path string, s *Session, body any, timeout time.Duration, target any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("cannot encode phone request")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", p.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("cannot build phone request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("newToken", s.Token)
	req.Header.Set("X-ES-HTTP-VERSION", "1")
	req.Header.Set("Connection", "keep-alive")
	if path == "/version" {
		req.Header.Set("version", "7.0.6")
	}
	response, err := p.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errCertificatePinMismatch) {
			return errCertificatePinMismatch
		}
		return errors.New("phone TLS/network request failed; verify its certificate pin and reachability")
	}
	if response.StatusCode == 445 || response.StatusCode == http.StatusForbidden {
		response.Body.Close()
		return ErrFilePermission
	}
	if target == nil {
		_, err := readFileAcknowledgment(response)
		return err
	}
	return readJSON(response, target)
}

func (p *PhoneClient) Negotiate(ctx context.Context, s *Session) (map[string]bool, error) {
	version := map[string]any{"version": "7.0.6", "connBaseVersionCode": 1155, "pcSuiteVersionCode": 70007, "timestamp": time.Now().UnixMilli(), "connectionId": s.ConnectionID, "pcDeviceId": s.Identity.DeviceID, "isAutoConnect": false, "token": s.Token, "isOversea": true, "pcOsType": "linux", "pcOsVersion": runtime.GOOS + "/" + runtime.GOARCH}
	var reply struct {
		Code string `json:"code"`
		Data struct {
			Suite    int `json:"pcSuiteVersionCode"`
			Connbase int `json:"connBaseVersionCode"`
			ENP      int `json:"enpVer"`
		} `json:"data"`
	}
	if err := p.post(ctx, "/version", s, version, 4*time.Second, &reply); err != nil {
		return nil, fmt.Errorf("phone version: %w", err)
	}
	if reply.Code != "0000" {
		return nil, errors.New("phone rejected version negotiation")
	}
	if reply.Data.Suite < 70007 || reply.Data.Connbase < 1155 || reply.Data.ENP > 1 {
		return nil, errors.New("phone component versions are incompatible with this protocol milestone")
	}
	base := map[string]any{"pc_name": s.HostName, "pcDeviceId": s.Identity.DeviceID, "isLogin": false, "openid": "", "pcLoginAccount": "", "isAutoConnect": false, "pcSystemType": "1", "isSupVdfs": false, "token": s.Token, "isPcOsSupportExtScreen": false, "pcBleId": s.Identity.DeviceID, "deviceAvatarUri": ""}
	var info struct {
		Code string                     `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := p.post(ctx, "/base-info", s, base, 2*time.Second, &info); err != nil {
		return nil, fmt.Errorf("phone base info: %w", err)
	}
	if info.Code != "0000" || info.Data == nil {
		return nil, errors.New("phone rejected base-info negotiation")
	}
	s.StorageRoot = ""
	var storageRoot string
	if json.Unmarshal(info.Data["innerRoot"], &storageRoot) == nil && storageRoot != "/" && validPhonePath(storageRoot) {
		s.StorageRoot = storageRoot
	}
	capabilities := make(map[string]bool)
	for _, key := range []string{"isSupportCall", "isSupportPcMode", "isSupportFmDelete", "isConnectAlbum", "isFmHasPermission", "hasStoryPermission", "isSupportBackup"} {
		var value bool
		if raw, ok := info.Data[key]; ok && json.Unmarshal(raw, &value) == nil {
			capabilities[key] = value
		}
	}
	return capabilities, nil
}

func (p *PhoneClient) Control(ctx context.Context, s *Session, event func(Event)) error {
	return p.control(ctx, s, 5*time.Second, event)
}

func (p *PhoneClient) ControlWithFeatures(ctx context.Context, s *Session, options ControlOptions, event func(Event)) error {
	return p.controlFeatures(ctx, s, 5*time.Second, options, event)
}

func (p *PhoneClient) control(ctx context.Context, s *Session, interval time.Duration, event func(Event)) error {
	return p.controlFeatures(ctx, s, interval, ControlOptions{}, event)
}

func (p *PhoneClient) controlFeatures(ctx context.Context, s *Session, interval time.Duration, options ControlOptions, event func(Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	conn, response, err := websocket.Dial(dialCtx, strings.Replace(p.base, "https://", "wss://", 1)+"/ws/heart-beat", &websocket.DialOptions{HTTPClient: p.http, Subprotocols: []string{"v1.hc.vivo.com.cn", s.Token}})
	dialCancel()
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("phone control WebSocket upgrade failed")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxResponseBytes)
	var mu sync.Mutex
	emit := func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		if event != nil {
			event(e)
		}
	}
	emit(Event{Time: time.Now().UTC(), Stage: "control_connected"})
	writeMessage := func(data []byte) error {
		c, stop := context.WithTimeout(ctx, 3*time.Second)
		defer stop()
		return conn.Write(c, websocket.MessageText, data)
	}
	write := func() error { return writeMessage([]byte(`{"state":"normal"}`)) }
	if err := write(); err != nil {
		return errors.New("initial control heartbeat failed")
	}
	emit(Event{Time: time.Now().UTC(), Stage: "heartbeat_sent", Count: 1})
	var clipboardChanges <-chan string
	var guard clipboardGuard
	if options.Clipboard != nil {
		clipboardChanges = options.Clipboard.Changes()
		// Discard events copied while the QR was waiting; sharing begins now.
		for len(clipboardChanges) > 0 {
			<-clipboardChanges
		}
		emit(Event{Time: time.Now().UTC(), Stage: "clipboard_enabled"})
	}
	if options.FilePermission != nil {
		if err := writeMessage([]byte("FILE_MANAGER:")); err != nil {
			return errors.New("file permission request failed")
		}
		emit(Event{Time: time.Now().UTC(), Stage: "file_permission_requested"})
	}
	if options.MirrorPermission != nil {
		if err := writeMessage([]byte("req_authrity")); err != nil {
			return errors.New("mirror permission request failed")
		}
		emit(Event{Time: time.Now().UTC(), Stage: "mirror_permission_requested"})
	}
	if options.Connected != nil {
		options.Connected()
	}
	var workers sync.WaitGroup
	workers.Add(1)
	defer workers.Wait()
	defer cancel()
	go func() {
		defer workers.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		count := 1
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if write() != nil {
					cancel()
					return
				}
				count++
				emit(Event{Time: time.Now().UTC(), Stage: "heartbeat_sent", Count: count})
			case text, ok := <-clipboardChanges:
				if !ok {
					clipboardChanges = nil
					emit(Event{Time: time.Now().UTC(), Stage: "clipboard_unavailable"})
					continue
				}
				if !guard.send(text) {
					continue
				}
				if writeMessage([]byte("CLIPBOARD_TEXT:"+text)) != nil {
					cancel()
					return
				}
				emit(Event{Time: time.Now().UTC(), Stage: "clipboard_to_phone", Count: len(text)})
			case _, ok := <-options.FilePermissionRequests:
				if !ok {
					options.FilePermissionRequests = nil
					continue
				}
				if writeMessage([]byte("FILE_MANAGER:")) != nil {
					cancel()
					return
				}
				emit(Event{Time: time.Now().UTC(), Stage: "file_permission_requested"})
			case request, ok := <-options.MainRequests:
				if !ok {
					options.MainRequests = nil
					continue
				}
				if len(request) == 0 || len(request) > 16*1024 || writeMessage(request) != nil {
					cancel()
					return
				}
			}
		}
	}()
	for {
		readCtx, stop := context.WithTimeout(ctx, 40*time.Second)
		typ, data, err := conn.Read(readCtx)
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				emit(Event{Time: time.Now().UTC(), Stage: "control_disconnected", Code: "peer_closed"})
				return nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || websocket.CloseStatus(err) == websocket.StatusAbnormalClosure {
				emit(Event{Time: time.Now().UTC(), Stage: "control_disconnected", Code: "transport_closed"})
				return ErrControlDisconnected
			}
			emit(Event{Time: time.Now().UTC(), Stage: "control_disconnected", Code: "read_failed_or_timed_out"})
			return errors.New("phone control disconnected or timed out")
		}
		if typ == websocket.MessageText {
			emit(SummarizeMessage(data))
			if options.CameraMessage != nil {
				options.CameraMessage(data)
			}
			if options.MirrorPermission != nil || options.MirrorPermissionState != nil {
				if allowed, ok := mirrorPermission(data); ok {
					if options.MirrorPermissionState != nil {
						options.MirrorPermissionState(allowed)
					}
					select {
					case options.MirrorPermission <- allowed:
					default:
					}
				}
			}
			if options.FilePermission != nil || options.FilePermissionState != nil {
				if allowed, ok := filePermission(data); ok {
					if options.FilePermissionState != nil {
						options.FilePermissionState(allowed)
					}
					stage := "file_permission_denied"
					if allowed {
						stage = "file_permission_granted"
					}
					emit(Event{Time: time.Now().UTC(), Stage: stage})
					select {
					case options.FilePermission <- allowed:
					default:
					}
				}
			}
			if options.Clipboard != nil {
				text, err := clipboardText(data)
				if err != nil {
					emit(Event{Time: time.Now().UTC(), Stage: "clipboard_rejected"})
					continue
				}
				if text != "" && guard.receive(text) {
					if err := options.Clipboard.Set(ctx, text); err != nil {
						emit(Event{Time: time.Now().UTC(), Stage: "clipboard_write_failed"})
					} else {
						emit(Event{Time: time.Now().UTC(), Stage: "clipboard_from_phone", Count: len(text)})
					}
				}
			}
		}
	}
}

// InspectCertificate retrieves public metadata only. No HTTP request or token is sent.
// The returned fingerprint is not independently authenticated; callers must verify it.
func InspectCertificate(ctx context.Context, address netip.Addr) (string, error) {
	if address.Zone() != "" {
		return "", errors.New("phone address must not contain an interface zone")
	}
	address = address.Unmap()
	if !address.IsPrivate() || address.Zone() != "" {
		return "", errors.New("phone must have a private LAN address")
	}
	return inspectCertificate(ctx, net.JoinHostPort(address.String(), "10380"))
}

func inspectCertificate(ctx context.Context, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var fingerprint string
	errMetadata := errors.New("certificate metadata collected")
	config := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("missing certificate")
		}
		digest := sha256.Sum256(state.PeerCertificates[0].Raw)
		fingerprint = hex.EncodeToString(digest[:])
		return errMetadata
	}}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: config}
	conn, err := dialer.DialContext(ctx, "tcp", endpoint)
	if conn != nil {
		conn.Close()
	}
	if fingerprint != "" && errors.Is(err, errMetadata) {
		return fingerprint, nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", errors.New("could not inspect phone TLS certificate; phone service may require a fresh scan first")
}
