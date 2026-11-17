package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vivolinux/internal/vivo"
)

func directFixture(t *testing.T) (*directBroker, directNetwork) {
	t.Helper()
	b := &directBroker{config: directConfig{Interface: "wlan0"}, token: "fixture-api-token", ready: make(chan struct{}, 1), peer: make(chan netip.Addr, 1),
		owns:   func(name, ip string) bool { return name == "p2p-vc-test" && ip == "192.168.49.1" },
		listen: func(_, _ string) (net.Listener, error) { return net.Listen("tcp4", "127.0.0.1:0") }}
	t.Cleanup(b.Close)
	return b, directNetwork{ID: "fixture-group", Station: "wlan0", Group: "p2p-vc-test", IP: "192.168.49.1", CIDR: "192.168.49.0/24", SSID: "DIRECT-fixture", Passphrase: "fixture-password", BSSID: "02:11:22:33:44:55", Persistent: true, Management: "system-service"}
}

func directRequest(b *directBroker, method, path, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", auth)
	w := httptest.NewRecorder()
	b.handleAPI(w, r)
	return w
}

func registerDirect(t *testing.T, b *directBroker, n directNetwork) {
	t.Helper()
	body, _ := json.Marshal(n)
	w := directRequest(b, "POST", "/api/p2p/register", string(body), "Bearer "+b.token)
	if w.Code != 200 {
		t.Fatalf("register: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), n.Passphrase) || strings.Contains(w.Body.String(), b.token) {
		t.Fatal("credential exposed")
	}
}

func TestDirectServiceProtocolAndQR(t *testing.T) {
	b, n := directFixture(t)
	if w := directRequest(b, "GET", "/api/p2p/status", "", "wrong"); w.Code != 401 {
		t.Fatal("unauthenticated API accepted")
	}
	registerDirect(t, b, n)
	release := b.authorizeAutomatic("bluetooth-window")
	waiting := directRequest(b, "POST", "/api/p2p/heartbeat", `{"id":"fixture-group"}`, "Bearer "+b.token)
	if !strings.Contains(waiting.Body.String(), `"reconnectRequested":false`) || !strings.Contains(waiting.Body.String(), `"pairingActive":true`) {
		t.Fatal("Bluetooth window incorrectly requested persistent invitations")
	}
	rAuto := httptest.NewRequest("GET", "/notifyConnection", nil)
	rAuto.RemoteAddr = "192.168.49.22:54321"
	wAuto := httptest.NewRecorder()
	b.handleCallback(wAuto, rAuto)
	if wAuto.Code != 403 {
		t.Fatal("automatic preconnection accepted an unsolicited QR callback")
	}
	release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got, err := b.waitNetwork(ctx); err != nil || got.ID != n.ID {
		t.Fatal("registration not delivered", err)
	}
	s, err := vivo.NewSession(vivo.Identity{DeviceID: strings.Repeat("a", 32)}, "Fixture PC", n.IP)
	if err != nil {
		t.Fatal(err)
	}
	s.Region = vivo.RegionChina
	uri, err := b.begin(s)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for k, v := range map[string]string{"ls": "true", "we": "true", "wid": n.SSID, "wpw": n.Passphrase, "wm": n.BSSID, "gw": n.IP, "t": s.Token, "id": s.Identity.DeviceID} {
		if q.Get(k) != v {
			t.Fatalf("wrong QR field %s", k)
		}
	}
	if u.Host != "psuite.vivo.com.cn" || q.Has("lip") || q.Get("s") == "" {
		t.Fatal("wrong direct QR branch")
	}
	w := directRequest(b, "POST", "/api/p2p/heartbeat", `{"id":"fixture-group","progress":{"authorizedStations":0}}`, "Bearer "+b.token)
	if !strings.Contains(w.Body.String(), `"pairingActive":true`) || !strings.Contains(w.Body.String(), s.ConnectionID) {
		t.Fatal("service cannot open pairing window", w.Body)
	}
	for _, remote := range []string{"10.1.2.3:54321", "192.168.49.1:54321", "192.168.49.255:54321"} {
		r := httptest.NewRequest("GET", "/notifyConnection?phoneIp=192.168.49.99", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		b.handleCallback(w, r)
		if w.Code != 403 {
			t.Fatal("foreign callback accepted", remote)
		}
	}
	r := httptest.NewRequest("GET", "/notifyConnection?phoneIp=8.8.8.8", nil)
	r.RemoteAddr = "192.168.49.22:54321"
	w = httptest.NewRecorder()
	b.handleCallback(w, r)
	if w.Code != 200 {
		t.Fatal("callback rejected", w.Code)
	}
	peer, err := b.waitPeer(ctx)
	if err != nil || len(peer.Addresses) != 1 || peer.Addresses[0].String() != "192.168.49.22" {
		t.Fatal("did not use callback sender", err)
	}
	w = httptest.NewRecorder()
	b.handleCallback(w, r)
	if w.Code != 403 {
		t.Fatal("callback replay accepted")
	}
	w = directRequest(b, "POST", "/api/p2p/heartbeat", `{"id":"fixture-group"}`, "Bearer "+b.token)
	if !strings.Contains(w.Body.String(), `"pairingActive":false`) {
		t.Fatal("pairing window stayed open")
	}
	b.Close()
	w = directRequest(b, "GET", "/api/p2p/status", "", "Bearer "+b.token)
	if w.Code != 503 {
		t.Fatal("closed broker accepted request")
	}
}

func TestDirectRejectsChangedOrForeignRegistration(t *testing.T) {
	b, n := directFixture(t)
	for _, change := range []func(*directNetwork){
		func(n *directNetwork) { n.IP = "10.0.0.1" }, func(n *directNetwork) { n.Group = n.Station },
		func(n *directNetwork) { n.CIDR = "192.168.49.0/16" }, func(n *directNetwork) { n.Persistent = false },
		func(n *directNetwork) { n.Group = "foreign0" }, func(n *directNetwork) { n.Passphrase = "short" },
	} {
		bad := n
		change(&bad)
		body, _ := json.Marshal(bad)
		if w := directRequest(b, "POST", "/api/p2p/register", string(body), "Bearer "+b.token); w.Code != 400 {
			t.Fatal("invalid registration accepted")
		}
	}
	registerDirect(t, b, n)
	body, _ := json.Marshal(n)
	if w := directRequest(b, "POST", "/api/p2p/register", string(body), "Bearer "+b.token); w.Code != 409 {
		t.Fatal("group replaced")
	}
	if w := directRequest(b, "POST", "/api/p2p/heartbeat", `{"id":"wrong-group"}`, "Bearer "+b.token); w.Code != 404 {
		t.Fatal("foreign heartbeat accepted")
	}
	b.mu.Lock()
	b.lease = time.Now().Add(-time.Second)
	b.mu.Unlock()
	s, _ := vivo.NewSession(vivo.Identity{DeviceID: "fixture"}, "PC", n.IP)
	if _, err := b.begin(s); err == nil {
		t.Fatal("expired service accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.waitPeer(ctx); err != context.Canceled {
		t.Fatal("wait ignored cancellation", err)
	}
}

func TestDirectConfigDoesNotFollowLinksOrReadPublicSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"apiToken":"fixture-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := readDirectConfigFile(path, uint32(os.Getuid()), true, &v); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	os.Symlink(path, link)
	if err := readDirectConfigFile(link, uint32(os.Getuid()), true, &v); err == nil {
		t.Fatal("followed symlink")
	}
	os.Chmod(path, 0644)
	if err := readDirectConfigFile(path, uint32(os.Getuid()), true, &v); err == nil {
		t.Fatal("read public credential file")
	}
}
