package vivo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestParseRegion(t *testing.T) {
	for value, want := range map[string]Region{"": RegionGlobal, "global": RegionGlobal, "IN": RegionGlobal, "cn": RegionChina, " China ": RegionChina} {
		got, err := ParseRegion(value)
		if err != nil || got != want {
			t.Fatalf("ParseRegion(%q) = %#v, %v", value, got, err)
		}
	}
	if _, err := ParseRegion("eu"); err == nil {
		t.Fatal("accepted unknown region")
	}
	if RegionChina.Gateway != "https://psuite.vivo.com.cn/vbusiness" || !RegionChina.Domestic || RegionGlobal.Domestic {
		t.Fatal("region contract changed")
	}
}

func TestChinaQRKeepsHTTPSAndVendorFieldOrder(t *testing.T) {
	session, err := NewSession(Identity{DeviceID: "0123456789abcdef0123456789abcdef"}, "Laptop & Linux", "10.1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	session.SID = "sid+/&=test"
	session.Domestic = true
	qr, err := session.QR(RegionChina.Gateway)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(qr, "https://psuite.vivo.com.cn/vbusiness?s=") {
		t.Fatalf("China QR must stay a plain https URL: %s", qr)
	}
	u, err := url.Parse(qr)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, pair := range strings.Split(u.RawQuery, "&") {
		keys = append(keys, strings.SplitN(pair, "=", 2)[0])
	}
	if strings.Join(keys, ",") != "s,f,t,u,d,mac,ls,pb,id" {
		t.Fatalf("QR field order differs from vendor: %v", keys)
	}
	q := u.Query()
	if q.Get("s") != "sid+/&=test" || q.Get("d") != "Lap****nux" || q.Get("t") != session.Token || q.Get("id") != session.Identity.DeviceID {
		t.Fatal("China QR lost protocol fields")
	}
	session.Domestic = false
	export, err := session.QR(RegionGlobal.Gateway)
	if err != nil || !strings.HasPrefix(export, "pcsuite:in-psuite.vivo.com/vbusiness?s=") {
		t.Fatalf("export QR changed: %s %v", export, err)
	}
}

func TestVersionAdvertisesRegion(t *testing.T) {
	for _, domestic := range []bool{false, true} {
		var oversea any
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			switch r.URL.Path {
			case "/version":
				oversea = body["isOversea"]
				fmt.Fprint(w, `{"code":"0000","data":{"pcSuiteVersionCode":70007,"connBaseVersionCode":3134,"enpVer":1}}`)
			case "/base-info":
				fmt.Fprint(w, `{"code":"0000","data":{"mobileDeviceName":"Phone"}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		digest := sha256.Sum256(server.Certificate().Raw)
		client, err := newPhoneClient(server.URL, hex.EncodeToString(digest[:]))
		if err != nil {
			t.Fatal(err)
		}
		s := &Session{Identity: Identity{DeviceID: "generated-id"}, HostName: "Linux", Token: "secret", ConnectionID: "c", Domestic: domestic}
		if _, err := client.Negotiate(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		server.Close()
		if oversea != !domestic {
			t.Fatalf("domestic=%v sent isOversea=%v", domestic, oversea)
		}
	}
}
