package remotepc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudEnrollmentAndPasswordContract(t *testing.T) {
	var c *Client
	var base string
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		var data any
		switch r.URL.Path {
		case "/remote-desk/v1/region/analysis":
			if r.Method != "GET" || r.URL.Query().Get("countryCode") != "IN" || r.URL.Query().Get("deviceType") != "1" || r.URL.Query().Get("env") != "prd" {
				t.Error("incorrect region request")
			}
			if value, err := c.cipher.Decrypt(r.URL.Query().Get("cipherText")); err != nil || value != "ok" {
				t.Error("missing encrypted proof")
			}
			data = Region{BackendDomain: base, WebsocketDomain: "wss://example.test", MonitorDomain: base, MachineArea: "IN"}
		case "/remote-desk/v1/device/maodun/regist":
			if r.Method != "POST" || r.Header.Get("countryCode") != "IN" {
				t.Error("incorrect registration headers")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["identity"] != strings.Repeat("a", 64) || body["pcDeviceId"] != "synthetic-suite-device" || body["deviceDefaultName"] != "Synthetic PC" {
				t.Error("incorrect registration body")
			}
			if value, err := c.cipher.Decrypt(body["cipherText"].(string)); err != nil || value != "ok" {
				t.Error("missing registration proof")
			}
			data = Device{DeviceNo: "1234567890", DeviceToken: "synthetic-device-token", TempPassword: c.cipher.Encrypt("123456"), DeviceType: 1}
		case "/remote-desk/v1/device/maodun/refreshTempPassword":
			if r.Method != "GET" || r.URL.Query().Get("deviceNo") != "1234567890" || r.Header.Get("deviceToken") != "synthetic-device-token" || r.Header.Get("countryCode") != "IN" {
				t.Error("incorrect password refresh request")
			}
			data = c.cipher.Encrypt("654321")
		case "/remote-desk/v1/sip/maodun/account":
			if r.Method != "GET" || r.Header.Get("deviceToken") != "synthetic-device-token" || r.URL.Query().Get("deviceNo") != "1234567890" {
				t.Error("incorrect SIP credential request")
			}
			if value, err := c.cipher.Decrypt(r.URL.Query().Get("cipherText")); err != nil || value != "ok" {
				t.Error("missing SIP proof")
			}
			data = SIPAccount{SIPUserName: "synthetic-sip-user", SIPPassword: c.cipher.Encrypt("synthetic-sip-password"), SIPDomain: "sip.example.test", SIPDomainProxy: "proxy.example.test:5060", STUNAddress: "stun.example.test:3478", CipherKey: c.cipher.Encrypt("synthetic-gcm-key"), IV: c.cipher.Encrypt("synthetic-gcm-iv"), AAD: c.cipher.Encrypt("synthetic-aad"), Version: 1}
		case "/remote-desk/v1/device/refreshToken":
			if r.Method != "GET" || r.Header.Get("deviceToken") != "synthetic-device-token" || r.URL.Query().Get("deviceToken") != "synthetic-device-token" {
				t.Error("incorrect token refresh request")
			}
			data = "synthetic-refreshed-token"
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}))
	defer server.Close()
	base = server.URL
	var err error
	c, err = NewClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.bootstrap = base
	region, err := c.ResolveRegion(context.Background(), "IN", 1)
	if err != nil {
		t.Fatal(err)
	}
	device, err := c.Register(context.Background(), region, Registration{DeviceType: 1, Identity: strings.Repeat("a", 64), DeviceDefaultName: "Synthetic PC", Version: "3.0.6", PCDeviceID: "synthetic-suite-device", CountryCode: "IN"})
	if err != nil || device.TempPassword != "123456" {
		t.Fatalf("register failed: %v", err)
	}
	password, err := c.RefreshPassword(context.Background(), region, device, "IN")
	if err != nil || password != "654321" {
		t.Fatalf("password refresh failed: %v", err)
	}
	account, err := c.GetSIPAccount(context.Background(), region, device)
	if err != nil || account.SIPPassword != "synthetic-sip-password" || account.CipherKey != "synthetic-gcm-key" || account.IV != "synthetic-gcm-iv" || account.AAD != "synthetic-aad" || account.Version != 1 {
		t.Fatalf("SIP decoding failed: %v", err)
	}
	token, err := c.RefreshToken(context.Background(), region, device)
	if err != nil || token != "synthetic-refreshed-token" {
		t.Fatalf("token refresh failed: %v", err)
	}
	if requests != 5 {
		t.Fatalf("got %d requests", requests)
	}
}

func TestCloudRejectsMalformedAndSensitiveErrors(t *testing.T) {
	for name, body := range map[string]string{
		"missing code":     `{"data":{}}`,
		"server rejection": `{"code":1002,"msg":"sensitive-response-value"}`,
		"null data":        `{"code":0,"data":null}`,
		"unsafe endpoint":  `{"code":0,"data":{"backendDomain":"http://example.test","websocketDomain":"wss://example.test"}}`,
		"oversized":        strings.Repeat("sensitive-response-value", 30000),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			c, err := NewClient(server.Client())
			if err != nil {
				t.Fatal(err)
			}
			c.bootstrap = server.URL
			_, err = c.ResolveRegion(context.Background(), "IN", 1)
			if err == nil || strings.Contains(err.Error(), "sensitive-response-value") {
				t.Fatalf("unsafe response error: %v", err)
			}
		})
	}
}
