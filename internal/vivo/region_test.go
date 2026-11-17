package vivo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRegionQRForBothTransports(t *testing.T) {
	for _, region := range []Region{RegionChina, RegionOverseas, ""} {
		t.Run(string(region), func(t *testing.T) {
			s, err := NewSession(Identity{DeviceID: "fixture-pc"}, "Fixture PC", "192.168.49.1")
			if err != nil {
				t.Fatal(err)
			}
			s.Region = region
			s.SID = "fixture-sid"
			lan, err := s.QR(region.Gateway())
			if err != nil {
				t.Fatal(err)
			}
			direct, err := s.DirectQR("DIRECT-fixture", "fixture-password", "02:11:22:33:44:55", "192.168.49.1")
			if err != nil {
				t.Fatal(err)
			}
			host := "in-psuite.vivo.com"
			scheme := "pcsuite:"
			if region == RegionChina {
				host = "psuite.vivo.com.cn"
				scheme = "https://"
			}
			for _, qr := range []string{lan, direct} {
				if !strings.HasPrefix(qr, scheme+host+"/vbusiness?") {
					t.Fatal("wrong regional QR handler", qr)
				}
				u, err := url.Parse("https://" + strings.TrimPrefix(qr, scheme))
				if err != nil {
					t.Fatal(err)
				}
				q := u.Query()
				if q.Get("t") != s.Token || q.Get("id") != s.Identity.DeviceID || q.Get("s") == "" {
					t.Fatal("lost pairing credentials")
				}
				if qr == direct && (q.Get("ls") != "true" || q.Get("wid") != "DIRECT-fixture" || q.Has("lip")) {
					t.Fatal("wrong direct branch")
				}
				if qr == lan && (q.Get("ls") != "false" || q.Has("wid")) {
					t.Fatal("wrong LAN branch")
				}
			}
		})
	}
	for _, value := range []string{"", "unknown", "https://other.example"} {
		if _, err := ParseRegion(value); err == nil {
			t.Fatal("accepted invalid region", value)
		}
	}
}

func TestRegionalPhoneHandshake(t *testing.T) {
	for _, region := range []Region{RegionChina, RegionOverseas} {
		t.Run(string(region), func(t *testing.T) {
			seenVersion, seenBase := false, false
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil {
					t.Error("invalid request")
				}
				switch r.URL.Path {
				case "/version":
					seenVersion = true
					if body["isOversea"] != region.Overseas() {
						t.Error("wrong regional handshake flag")
					}
					fmt.Fprint(w, `{"code":"0000","data":{"pcSuiteVersionCode":70007,"connBaseVersionCode":3134,"enpVer":1}}`)
				case "/base-info":
					seenBase = true
					ble := "fixture-pc"
					if region == RegionChina {
						ble = ""
					}
					if body["pcBleId"] != ble || body["isLogin"] != false {
						t.Error("wrong anonymous regional base-info")
					}
					fmt.Fprint(w, `{"code":"0000","data":{}}`)
				default:
					t.Error("unexpected request")
				}
			}))
			defer server.Close()
			client, err := newPhoneClientTrust(server.URL, "", true)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			s := &Session{Region: region, Identity: Identity{DeviceID: "fixture-pc"}, Token: "fixture-token"}
			if _, err := client.Negotiate(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if !seenVersion || !seenBase {
				t.Fatal("incomplete handshake")
			}
		})
	}
}
