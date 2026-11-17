package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vivolinux/internal/vivo"
)

func TestBluetoothSignatureMatchesVivoConnectIndependentVector(t *testing.T) {
	peer := preconnectPeer{"phone-guid-example", "phone-random-example", "02:11:22:33:44:55", "account-example"}
	sign, err := bluetoothSignature(peer, "account-example", "connection-example", "token-example", "pc-seed-example")
	expected := "ed2cab7b8a3be8d5230dccb79fd757b06f957bff7cd984b71910d78b74d49ee9117f40c955862e16ee5d237405edea4df2dc47ff0f0d8b71a6911926cebc505b"
	if err != nil || sign != expected {
		t.Fatal("incompatible Bluetooth signature", err)
	}
	if _, err := bluetoothSignature(peer, "ambiguous|account", "connection", "token", "seed"); err == nil {
		t.Fatal("accepted ambiguous signed fields")
	}
}

func TestBluetoothRadioErrorsDoNotTreatTemporaryDisconnectAsLostPairing(t *testing.T) {
	for _, code := range []string{"BLUETOOTH_DISCONNECTED", "BLUETOOTH_TIMEOUT", "Failed"} {
		if !errors.Is(bluetoothFailure(code, ""), errBluetoothTemporary) {
			t.Fatal("transient Bluetooth failure disabled auto reconnect")
		}
	}
	for _, code := range []string{"BLUETOOTH_OFF", "BLUETOOTH_DEVICE_REQUIRED", "BLUETOOTH_UNAVAILABLE"} {
		if !errors.Is(bluetoothFailure(code, ""), errBluetoothRequired) {
			t.Fatal("missing radio/pairing did not request user action")
		}
	}
	if !errors.Is(bluetoothFailure("Failed", "br-connection-key-missing"), errBluetoothRequired) {
		t.Fatal("invalid pairing keys were retried")
	}
}

func TestBluetoothDecoderFragmentationChannelsAndBounds(t *testing.T) {
	frame, err := encodeBT(map[string]any{"cmd": "device_info", "reqId": "id", "name": "测试手机"})
	if err != nil {
		t.Fatal(err)
	}
	if string(frame[14:18]) != "#pcc" {
		t.Fatal("missing PcPreConnect channel")
	}
	decoder := btDecoder{}
	var all []preconnectFrame
	for _, b := range frame {
		out, err := decoder.feed([]byte{b})
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, out...)
	}
	if len(all) != 1 || all[0].Channel != "#pcc" || len(decoder.buffer) != 0 {
		t.Fatal("fragmented frame lost")
	}
	if out, err := (&btDecoder{}).feed(append(bytes.Clone(frame), frame...)); err != nil || len(out) != 2 {
		t.Fatal("coalesced frames lost")
	}
	bad := bytes.Clone(frame)
	binary.BigEndian.PutUint32(bad[10:], 1024*1024+1)
	if _, err := (&btDecoder{}).feed(bad); err == nil {
		t.Fatal("oversized frame accepted")
	}
	bad = bytes.Clone(frame)
	bad[0] = 0
	if _, err := (&btDecoder{}).feed(bad); err == nil {
		t.Fatal("bad magic accepted")
	}
	bad = bytes.Clone(frame)
	bad[14:18][3] = 'x'
	if _, err := (&btDecoder{}).feed(bad); err == nil {
		t.Fatal("unknown channel accepted")
	}
}

type fakeBTChannel struct {
	messages   []map[string]any
	account    string
	outcome    string
	address    string
	stale      bool
	closed     bool
	authorized *bool
}

func (f *fakeBTChannel) send(value any) error {
	m := value.(map[string]any)
	if m["cmd"] == "connect_primary" && !*f.authorized {
		return errors.New("sent group credentials outside pairing window")
	}
	f.messages = append(f.messages, m)
	return nil
}
func (f *fakeBTChannel) wait(ctx context.Context, command, id string) (map[string]json.RawMessage, error) {
	m := map[string]any{"cmd": command, "reqId": id, "code": "0000", "data": map[string]string{"openId": f.account}}
	if command == "connect_primary" {
		m["data"] = map[string]string{"userDialogSelectResult": f.outcome}
		m["localAddress"] = f.address
		if f.stale {
			m["reqId"] = "old-request"
		}
	}
	b, _ := json.Marshal(m)
	var out map[string]json.RawMessage
	json.Unmarshal(b, &out)
	return out, nil
}
func (f *fakeBTChannel) close() { f.closed = true }

func TestOfficialPreconnectAuthorizesBluetoothThenUsesFreshGroupAddress(t *testing.T) {
	for _, outcome := range []string{"agree", "busy", "refuse", "stale", "foreign", "account-mismatch"} {
		t.Run(outcome, func(t *testing.T) {
			account := vivoAccount{"account", "account-token", "login-device-id", "CN"}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("token") != account.Token || r.Header.Get("userId") != account.OpenID || r.Header.Get("deviceId") != account.DeviceID {
					t.Error("directory login headers changed")
				}
				fmt.Fprint(w, `{"code":0,"data":[{"type":1,"userId":"account","deviceId":"phone-guid","name":"Phone","bluetooth":"02:11:22:33:44:55","ext":{"randomId":"real-random-id"}}]}`)
			}))
			defer server.Close()
			authorized := false
			releaseCount := 0
			channel := &fakeBTChannel{account: "account", outcome: outcome, address: "192.168.50.22", authorized: &authorized}
			if outcome == "foreign" {
				channel.outcome = "agree"
				channel.address = "10.1.2.3"
			}
			if outcome == "stale" {
				channel.outcome = "agree"
				channel.stale = true
			}
			if outcome == "account-mismatch" {
				channel.account = "other-account"
			}
			p := officialPreconnector{server.Client(), server.URL, func(ctx context.Context, peer preconnectPeer, power bool) (btChannel, error) {
				if peer.RandomID != "real-random-id" || peer.AccountID != account.OpenID {
					t.Error("used unverified phone discovery identifiers")
				}
				return channel, nil
			}}
			s, _ := vivo.NewSession(vivo.Identity{DeviceID: "pc"}, "PC", "192.168.50.1")
			s.Token = "old-token"
			r := rememberedPhone{DeviceID: "phone-guid", DeviceName: "Phone", PhoneIP: "192.168.49.22"}
			n := directNetwork{IP: "192.168.50.1", SSID: "DIRECT-new", Passphrase: "new-password", BSSID: "02:00:00:00:00:01"}
			address, peer, finish, err := p.connect(context.Background(), account, r, s, n, false, func(id string) func() { authorized = true; return func() { authorized = false; releaseCount++ } })
			if outcome != "agree" {
				if err == nil || finish != nil || !channel.closed || authorized {
					t.Fatal("rejected preconnection became successful")
				}
				if outcome == "account-mismatch" && len(channel.messages) != 1 {
					t.Fatal("sent credentials before same-account authentication")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if address.String() != "192.168.50.22" || peer.DeviceID != "phone-guid" || s.Token == "old-token" || s.AccountOpenID != "account" || !s.AutoConnect {
				t.Fatal("used stale session data")
			}
			if !authorized || channel.closed {
				t.Fatal("pairing window/channel closed before HTTPS completion")
			}
			param := channel.messages[1]["param"].(map[string]any)
			if param["ssid"] != n.SSID || param["password"] != n.Passphrase || param["isAutoConnect"] != "1" || param["pcPcsuiteVersion"] != 7060 {
				t.Fatal("phone DTO mismatch")
			}
			var extra map[string]any
			json.Unmarshal([]byte(param["extra_info"].(string)), &extra)
			if extra["token"] != s.Token || extra["connectType"] != float64(3) {
				t.Fatal("signature context changed")
			}
			finish()
			finish()
			if authorized || !channel.closed || releaseCount != 1 {
				t.Fatal("preconnection cleanup is not idempotent")
			}
		})
	}
}

func TestCachedBluetoothPeerCannotCrossAccounts(t *testing.T) {
	p := defaultPreconnector()
	r := rememberedPhone{Preconnect: &preconnectPeer{"guid", "random", "02:11:22:33:44:55", "other-account"}}
	_, err := p.peer(context.Background(), vivoAccount{OpenID: "account"}, r)
	if !errors.Is(err, errLoginRequired) || strings.Contains(err.Error(), "other-account") {
		t.Fatal("cached identity crossed accounts or leaked")
	}
}
