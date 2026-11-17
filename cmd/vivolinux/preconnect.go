package main

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"vivolinux/internal/vivo"
)

var errBluetoothRequired = errors.New("pair this phone in system Bluetooth settings and enable Bluetooth")
var errPreconnectBusy = errors.New("phone preconnection busy or refused; wait before retrying")
var errBluetoothTemporary = errors.New("phone Bluetooth connection is temporarily unavailable")

func bluetoothFailure(code, reason string) error {
	if strings.Contains(reason, "br-connection-key-missing") {
		return errBluetoothRequired
	}
	switch code {
	case "BLUETOOTH_DISCONNECTED", "BLUETOOTH_TIMEOUT", "BLUETOOTH_IO_FAILED", "Failed", "InProgress", "NotAvailable":
		return errBluetoothTemporary
	default:
		return errBluetoothRequired
	}
}

type preconnectPeer struct {
	DeviceID  string `json:"deviceId"`
	RandomID  string `json:"randomId"`
	Bluetooth string `json:"bluetooth"`
	AccountID string `json:"accountOpenId"`
}

func (p preconnectPeer) valid() bool {
	for _, v := range []string{p.DeviceID, p.RandomID, p.AccountID} {
		if v == "" || len(v) > 512 || strings.ContainsAny(v, "\x00\r\n") {
			return false
		}
	}
	mac, e := net.ParseMAC(p.Bluetooth)
	return e == nil && len(mac) == 6
}

func requestID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic("system randomness unavailable")
	}
	return hex.EncodeToString(b)
}

func bluetoothSignature(peer preconnectPeer, openID, connection, token, seed string) (string, error) {
	if !peer.valid() || peer.AccountID!=openID || seed == "" || len(seed) > 512 {
		return "", errors.New("invalid preconnection identity")
	}
	for _, v := range []string{openID, connection, token} {
		if v == "" || strings.ContainsAny(v, "|\x00\r\n") {
			return "", errors.New("invalid signed field")
		}
	}
	key := sha256.Sum256([]byte(peer.DeviceID + peer.RandomID + seed))
	block, _ := aes.NewCipher(key[:])
	plain := []byte(openID + "|" + connection + "|" + token)
	padding := aes.BlockSize - len(plain)%aes.BlockSize
	for range padding {
		plain = append(plain, byte(padding))
	}
	encrypted := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(encrypted, plain)
	return hex.EncodeToString(encrypted), nil
}

type preconnectFrame struct {
	Channel string
	Message map[string]json.RawMessage
}

func matchingBTReply(message map[string]json.RawMessage, cmd, id string) bool {
	var actualCommand, actualID string
	return json.Unmarshal(message["cmd"], &actualCommand) == nil && json.Unmarshal(message["reqId"], &actualID) == nil && actualCommand == cmd && actualID == id
}

func encodeBT(message any) ([]byte, error) {
	data, err := json.Marshal(message)
	if err != nil || len(data) > 1024*1024-4 {
		return nil, errors.New("invalid preconnection message")
	}
	body := append([]byte("#pcc"), data...)
	frame := append([]byte("PAY_LOAD_1"), make([]byte, 4)...)
	binary.BigEndian.PutUint32(frame[10:], uint32(len(body)))
	return append(frame, body...), nil
}

type btDecoder struct{ buffer []byte }

func (d *btDecoder) feed(chunk []byte) ([]preconnectFrame, error) {
	d.buffer = append(d.buffer, chunk...)
	if len(d.buffer) > 2*1024*1024 {
		return nil, errors.New("oversized Bluetooth input")
	}
	var out []preconnectFrame
	for len(d.buffer) >= 14 {
		if string(d.buffer[:10]) != "PAY_LOAD_1" {
			return nil, errors.New("invalid Bluetooth frame")
		}
		size := int(binary.BigEndian.Uint32(d.buffer[10:14]))
		if size < 1 || size > 1024*1024 {
			return nil, errors.New("invalid Bluetooth frame length")
		}
		if len(d.buffer) < 14+size {
			break
		}
		body := d.buffer[14 : 14+size]
		d.buffer = d.buffer[14+size:]
		channel := ""
		if body[0] == '#' {
			if len(body) < 4 {
				return nil, errors.New("invalid Bluetooth channel")
			}
			channel = string(body[:4])
			body = body[4:]
			if channel != "#bri" && channel != "#inn" && channel != "#ble" && channel != "#pcc" {
				return nil, errors.New("unknown Bluetooth channel")
			}
		}
		var message map[string]json.RawMessage
		if !utf8.Valid(body) || json.Unmarshal(body, &message) != nil || message == nil {
			return nil, errors.New("invalid Bluetooth JSON")
		}
		out = append(out, preconnectFrame{channel, message})
	}
	return out, nil
}

type btChannel interface {
	send(any) error
	wait(context.Context, string, string) (map[string]json.RawMessage, error)
	close()
}
type btProcess struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	cancel  context.CancelFunc
	done    chan struct{}
	ready   chan struct{}
	frames  chan preconnectFrame
	failure chan error
	once    sync.Once
}

func openBluetooth(ctx context.Context, peer preconnectPeer, powerOn bool) (btChannel, error) {
	json.NewEncoder(os.Stdout).Encode(vivo.Event{Time: time.Now().UTC(), Stage: "bluetooth_attempt"})
	exe, err := os.Executable()
	if err != nil {
		return nil, errBluetoothRequired
	}
	childCtx, cancel := context.WithTimeout(ctx, 65*time.Second)
	p := &btProcess{cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}, 1), frames: make(chan preconnectFrame, 32), failure: make(chan error, 1)}
	p.cmd = exec.CommandContext(childCtx, filepath.Join(filepath.Dir(exe), "vivo-bluetooth-helper"))
	p.input, err = p.cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errBluetoothRequired
	}
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, errBluetoothRequired
	}
	if err = p.cmd.Start(); err != nil {
		cancel()
		return nil, errBluetoothRequired
	}
	go func() {
		defer close(p.done)
		defer p.cmd.Wait()
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 65536), 3*1024*1024)
		decoder := btDecoder{}
		fail := func(err error) {
			select {
			case p.failure <- err:
			default:
			}
		}
		for scanner.Scan() {
			var event struct {
				Type   string `json:"type"`
				Data   string `json:"data"`
				Code   string `json:"code"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				fail(errBluetoothRequired)
				return
			}
			switch event.Type {
			case "ready":
				select {
				case p.ready <- struct{}{}:
				default:
				}
			case "error":
				fail(bluetoothFailure(event.Code, event.Reason))
				return
			case "data":
				chunk, e := base64.StdEncoding.DecodeString(event.Data)
				if e != nil {
					fail(errBluetoothRequired)
					return
				}
				frames, e := decoder.feed(chunk)
				if e != nil {
					fail(e)
					return
				}
				for _, frame := range frames {
					select {
					case p.frames <- frame:
					case <-childCtx.Done():
						return
					}
				}
			}
		}
		fail(errBluetoothTemporary)
	}()
	err = json.NewEncoder(p.input).Encode(map[string]any{"address": peer.Bluetooth, "powerOn": powerOn, "timeout": 60})
	if err == nil {
		select {
		case <-p.ready:
			return p, nil
		case err = <-p.failure:
		case <-childCtx.Done():
			err = errBluetoothTemporary
		}
	}
	p.close()
	return nil, err
}
func (p *btProcess) send(message any) error {
	frame, err := encodeBT(message)
	if err != nil {
		return err
	}
	if json.NewEncoder(p.input).Encode(map[string]string{"send": base64.StdEncoding.EncodeToString(frame)}) != nil {
		return errBluetoothRequired
	}
	return nil
}
func (p *btProcess) wait(ctx context.Context, command, id string) (map[string]json.RawMessage, error) {
	for {
		select {
		case frame := <-p.frames:
			var cmd, req string
			json.Unmarshal(frame.Message["cmd"], &cmd)
			json.Unmarshal(frame.Message["reqId"], &req)
			if frame.Channel == "" && cmd == command && req == id {
				return frame.Message, nil
			}
		case err := <-p.failure:
			return nil, err
		case <-ctx.Done():
			return nil, errPreconnectBusy
		}
	}
}
func (p *btProcess) close() { p.once.Do(func() { p.input.Close(); p.cancel(); <-p.done }) }

type officialPreconnector struct {
	client    *http.Client
	directory string
	open      func(context.Context, preconnectPeer, bool) (btChannel, error)
}

func defaultPreconnector() officialPreconnector {
	return officialPreconnector{client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, directory: "https://connection-center.vivo.com.cn/device/list", open: openBluetooth}
}

func (p officialPreconnector) peer(ctx context.Context, a vivoAccount, r rememberedPhone) (preconnectPeer, error) {
	if r.Preconnect != nil && r.Preconnect.AccountID != a.OpenID {
		return preconnectPeer{}, errLoginRequired
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", p.directory, nil)
	if a.RegionCode == "" {
		a.RegionCode = "CN"
	}
	for key, value := range map[string]string{"token": a.Token, "userId": a.OpenID, "deviceId": a.DeviceID, "source": "2", "version": "7.0.6.0", "countryCode": a.RegionCode} {
		req.Header.Set(key, value)
	}
	response, err := p.client.Do(req)
	if err != nil {
		if r.Preconnect != nil && r.Preconnect.valid() && r.Preconnect.AccountID == a.OpenID && ctx.Err() == nil {
			return *r.Preconnect, nil
		}
		return preconnectPeer{}, errors.New("vivo device discovery unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return preconnectPeer{}, errLoginRequired
	}
	var list struct {
		Code *int `json:"code"`
		Data []struct {
			Type      int    `json:"type"`
			UserID    string `json:"userId"`
			DeviceID  string `json:"deviceId"`
			Name      string `json:"name"`
			Bluetooth string `json:"bluetooth"`
			Ext       struct {
				RandomID string `json:"randomId"`
			} `json:"ext"`
		} `json:"data"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 || json.Unmarshal(data, &list) != nil || list.Code == nil || *list.Code != 0 {
		return preconnectPeer{}, errLoginRequired
	}
	var matches []preconnectPeer
	for _, d := range list.Data {
		if d.Type != 1 || d.UserID != a.OpenID {
			continue
		}
		match := d.DeviceID == r.DeviceID
		if r.Preconnect != nil {
			match = d.DeviceID == r.Preconnect.DeviceID
		} else if !match {
			match = r.DeviceName != "" && d.Name == r.DeviceName
		}
		candidate := preconnectPeer{d.DeviceID, d.Ext.RandomID, d.Bluetooth, a.OpenID}
		if match && candidate.valid() {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return preconnectPeer{}, errors.New("no unique matching phone in the vivo account device directory")
	}
	return matches[0], nil
}

func (p officialPreconnector) connect(ctx context.Context, a vivoAccount, r rememberedPhone, s *vivo.Session, n directNetwork, powerOn bool, authorize func(string) func()) (netip.Addr, *preconnectPeer, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 65*time.Second)
	peer, err := p.peer(ctx, a, r)
	if err != nil {
		cancel()
		return netip.Addr{}, nil, nil, err
	}
	channel, err := p.open(ctx, peer, powerOn)
	if err != nil {
		cancel()
		return netip.Addr{}, nil, nil, err
	}
	var once sync.Once
	release := func() {}
	finish := func() { once.Do(func() { release(); channel.close(); cancel() }) }
	fail := func(err error) (netip.Addr, *preconnectPeer, func(), error) {
		finish()
		return netip.Addr{}, nil, nil, err
	}
	id := requestID()
	info := map[string]any{"version": "1", "reqId": id, "cmd": "device_info", "param": map[string]any{"deviceId": s.Identity.DeviceID, "deviceName": s.HostName, "accountName": "", "openId": a.OpenID, "deviceType": "pc", "isConnectNow": true}}
	if err = channel.send(info); err != nil {
		return fail(err)
	}
	infoCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	answer, err := channel.wait(infoCtx, "device_info", id)
	stop()
	if err != nil {
		return fail(err)
	}
	var code string
	var identity struct {
		OpenID string `json:"openId"`
	}
	json.Unmarshal(answer["code"], &code)
	json.Unmarshal(answer["data"], &identity)
	if !matchingBTReply(answer, "device_info", id) || code != "0000" || identity.OpenID != a.OpenID {
		return fail(errLoginRequired)
	}
	fresh, err := vivo.NewSession(s.Identity, s.HostName, n.IP)
	if err != nil {
		return fail(err)
	}
	s.Token = fresh.Token
	s.ConnectionID = fresh.ConnectionID
	s.AccountOpenID = a.OpenID
	s.AutoConnect = true
	release = authorize(s.ConnectionID)
	seed := requestID()
	sign, err := bluetoothSignature(peer, a.OpenID, s.ConnectionID, s.Token, seed)
	if err != nil {
		return fail(err)
	}
	extra, _ := json.Marshal(map[string]any{"connectType": 3, "seed": seed, "sign": sign, "token": s.Token})
	id = requestID()
	request := map[string]any{"version": "1", "reqId": id, "cmd": "connect_primary", "param": map[string]any{"deviceId": s.Identity.DeviceID, "deviceName": s.HostName, "accountName": "", "openId": a.OpenID, "deviceType": "pc", "connect_type": "1", "ssid": n.SSID, "password": n.Passphrase, "deviceAddress": n.BSSID, "isAutoConnect": "1", "pcPcsuiteVersion": 7060, "extra_info": string(extra)}}
	if err = channel.send(request); err != nil {
		return fail(err)
	}
	primaryCtx, stop := context.WithTimeout(ctx, 35*time.Second)
	answer, err = channel.wait(primaryCtx, "connect_primary", id)
	stop()
	if err != nil {
		return fail(err)
	}
	var outcome struct {
		Result string `json:"userDialogSelectResult"`
	}
	var address string
	json.Unmarshal(answer["code"], &code)
	json.Unmarshal(answer["data"], &outcome)
	json.Unmarshal(answer["localAddress"], &address)
	if !matchingBTReply(answer, "connect_primary", id) || code != "0000" || outcome.Result != "agree" {
		return fail(errPreconnectBusy)
	}
	phone, err := netip.ParseAddr(address)
	gateway, _ := netip.ParseAddr(n.IP)
	if err != nil || !phone.Is4() || phone == gateway || !netip.PrefixFrom(gateway, 24).Contains(phone) || phone.As4()[3] == 0 || phone.As4()[3] == 255 {
		return fail(errors.New("phone returned an invalid direct address"))
	}
	return phone, &peer, finish, nil
}
