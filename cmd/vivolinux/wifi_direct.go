package main

// Compatibility endpoint for the installed vivo-connect-p2p.service. The
// privileged service owns the radio, supplicant, DHCP and persistent profile;
// VOkit only receives its registration and opens a bounded pairing window.
import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"vivolinux/internal/vivo"
)

const directServiceConfig = "/etc/vivo-connect-p2p/service.json"

var errDirectPortBusy = errors.New("Wi-Fi Direct backend port is busy; quit vivo Connect and its backend, keep vivo-connect-p2p.service running, then retry")

type directConfig struct {
	UID       int    `json:"uid"`
	Port      int    `json:"port"`
	StateDir  string `json:"state_dir"`
	Interface string `json:"interface"`
}

func readDirectConfigFile(path string, uid uint32, private bool, value any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("cannot read Wi-Fi Direct service configuration; enable the persistent service in vivo Connect first")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return errors.New("cannot inspect Wi-Fi Direct configuration")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	mask := os.FileMode(0022)
	if private {
		mask = 0077
	}
	if !ok || owner.Uid != uid || !info.Mode().IsRegular() || info.Mode().Perm()&mask != 0 || info.Size() > 65536 {
		return errors.New("Wi-Fi Direct configuration has unsafe ownership, permissions or size")
	}
	b, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(b) > 65536 || json.Unmarshal(b, value) != nil {
		return errors.New("invalid Wi-Fi Direct configuration")
	}
	return nil
}

func installedDirectConfig() (directConfig, string, error) {
	var c directConfig
	if err := readDirectConfigFile(directServiceConfig, 0, false, &c); err != nil {
		return c, "", err
	}
	if c.UID != os.Getuid() || c.UID <= 0 || c.Port < 1 || c.Port > 65535 || !filepath.IsAbs(c.StateDir) || !directInterface.MatchString(c.Interface) {
		return c, "", errors.New("Wi-Fi Direct persistent service must be configured for this user and a valid interface/port")
	}
	var user struct {
		Token string `json:"apiToken"`
	}
	if err := readDirectConfigFile(filepath.Join(c.StateDir, "config.json"), uint32(c.UID), true, &user); err != nil {
		return c, "", err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9._~-]{4,256}$`).MatchString(user.Token) {
		return c, "", errors.New("invalid Wi-Fi Direct API credential")
	}
	return c, user.Token, nil
}

var directInterface = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,14}$`)
var directID = regexp.MustCompile(`^[\w-]{1,64}$`)

type directNetwork struct {
	ID         string `json:"id"`
	Station    string `json:"stationInterface"`
	Group      string `json:"groupInterface"`
	IP         string `json:"ip"`
	CIDR       string `json:"cidr"`
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	BSSID      string `json:"bssid"`
	Persistent bool   `json:"persistentGroup"`
	Management string `json:"management"`
}

func (n directNetwork) valid(station string) bool {
	ip, err := netip.ParseAddr(n.IP)
	if err != nil || !ip.Is4() {
		return false
	}
	b := ip.As4()
	mac, err := net.ParseMAC(n.BSSID)
	return err == nil && len(mac) == 6 && directID.MatchString(n.ID) && n.Station == station && n.Group != n.Station && directInterface.MatchString(n.Group) &&
		b[0] == 192 && b[1] == 168 && b[3] == 1 && n.CIDR == netip.PrefixFrom(ip, 24).Masked().String() &&
		len(n.SSID) >= 7 && len(n.SSID) <= 32 && n.SSID[:7] == "DIRECT-" && len(n.Passphrase) >= 8 && len(n.Passphrase) <= 63 && n.Persistent && n.Management == "system-service"
}

func directInterfaceOwns(name, ip string) bool {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return false
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addresses {
		p, err := netip.ParsePrefix(a.String())
		if err == nil && p.Bits() == 24 && p.Addr().String() == ip {
			return true
		}
	}
	return false
}

type directBroker struct {
	mu            sync.Mutex
	config        directConfig
	token         string
	network       directNetwork
	lease         time.Time
	api, callback *http.Server
	ready         chan struct{}
	peer          chan netip.Addr
	pairingID     string
	pairingUntil  time.Time
	closed        bool
	automatic     bool
	// Dependency seam for offline protocol tests.
	owns   func(string, string) bool
	listen func(string, string) (net.Listener, error)
}

func boundedDirectServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
}

func startDirectBroker() (*directBroker, error) {
	c, token, err := installedDirectConfig()
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", c.Port))
	if err != nil {
		return nil, errDirectPortBusy
	}
	b := &directBroker{config: c, token: token, ready: make(chan struct{}, 1), peer: make(chan netip.Addr, 1), owns: directInterfaceOwns, listen: net.Listen}
	b.api = boundedDirectServer(http.HandlerFunc(b.handleAPI))
	go b.api.Serve(l)
	return b, nil
}

func directReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func (b *directBroker) handleAPI(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+b.token)) != 1 {
		directReply(w, 401, map[string]bool{"ready": false})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		directReply(w, 503, map[string]bool{"ready": false})
		return
	}
	status := func() map[string]any {
		active := b.pairingID != "" && time.Now().Before(b.pairingUntil) && time.Now().Before(b.lease)
		return map[string]any{"ready": b.network.ID != "" && time.Now().Before(b.lease), "pairingActive": active, "pairingId": b.pairingID, "reconnectRequested": false}
	}
	if r.Method == "GET" && r.URL.Path == "/api/p2p/status" {
		directReply(w, 200, status())
		return
	}
	if r.Method != "POST" {
		directReply(w, 404, status())
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 65536))
	if err != nil {
		directReply(w, 400, status())
		return
	}
	switch r.URL.Path {
	case "/api/p2p/register":
		var n directNetwork
		if json.Unmarshal(data, &n) != nil || !n.valid(b.config.Interface) || !b.owns(n.Group, n.IP) {
			directReply(w, 400, status())
			return
		}
		// Never replace a registered group during this pairing/session.
		if b.network.ID != "" {
			directReply(w, 409, status())
			return
		}
		l, err := b.listen("tcp4", net.JoinHostPort(n.IP, "9199"))
		if err != nil {
			directReply(w, 409, status())
			return
		}
		b.network = n
		b.lease = time.Now().Add(15 * time.Second)
		b.callback = boundedDirectServer(http.HandlerFunc(b.handleCallback))
		go b.callback.Serve(l)
		select {
		case b.ready <- struct{}{}:
		default:
		}
	case "/api/p2p/heartbeat", "/api/p2p/unregister":
		var body struct {
			ID       string `json:"id"`
			Progress struct {
				Stations int `json:"authorizedStations"`
			} `json:"progress"`
		}
		if json.Unmarshal(data, &body) != nil || b.network.ID == "" || body.ID != b.network.ID {
			directReply(w, 404, status())
			return
		}
		if r.URL.Path == "/api/p2p/unregister" {
			b.lease = time.Time{}
			b.pairingUntil = time.Time{}
		} else {
			b.lease = time.Now().Add(15 * time.Second)
		}
	default:
		directReply(w, 404, status())
		return
	}
	directReply(w, 200, status())
}

func (b *directBroker) authorizeAutomatic(id string) func() {
	b.mu.Lock()
	b.automatic = true
	b.pairingID = id
	b.pairingUntil = time.Now().Add(60 * time.Second)
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.pairingID == id {
			b.automatic = false
			b.pairingID = ""
			b.pairingUntil = time.Time{}
		}
	}
}

func (b *directBroker) waitNetwork(ctx context.Context) (directNetwork, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return directNetwork{}, errors.New("Wi-Fi Direct service did not register; check vivo-connect-p2p.service and its configured interface")
	case <-b.ready:
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.network, nil
	}
}

func (b *directBroker) begin(s *vivo.Session) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || !time.Now().Before(b.lease) {
		return "", errors.New("Wi-Fi Direct service registration expired")
	}
	uri, err := s.DirectQR(b.network.SSID, b.network.Passphrase, b.network.BSSID, b.network.IP)
	if err != nil {
		return "", err
	}
	b.pairingID = s.ConnectionID
	b.pairingUntil = time.Now().Add(180 * time.Second)
	return uri, nil
}

func (b *directBroker) handleCallback(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip, parseErr := netip.ParseAddr(host)
	gw, _ := netip.ParseAddr(b.network.IP)
	if r.Method != "GET" || r.URL.Path != "/notifyConnection" {
		directReply(w, 404, map[string]string{"code": "000001"})
		return
	}
	if err != nil || parseErr != nil || !ip.Is4() || ip == gw || !netip.PrefixFrom(gw, 24).Masked().Contains(ip) || ip.As4()[3] == 0 || ip.As4()[3] == 255 ||
		b.closed || b.automatic || b.pairingID == "" || !time.Now().Before(b.pairingUntil) || !time.Now().Before(b.lease) {
		directReply(w, 403, map[string]string{"code": "000001"})
		return
	}
	// Ignore phoneIp in the query: use only the sender on the registered group.
	b.pairingUntil = time.Time{}
	b.peer <- ip
	directReply(w, 200, map[string]string{"code": "000000", "data": "ok"})
}

func (b *directBroker) waitPeer(ctx context.Context) (vivo.Peer, error) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case ip := <-b.peer:
			return vivo.Peer{Addresses: []netip.Addr{ip}}, nil
		case <-ctx.Done():
			return vivo.Peer{}, ctx.Err()
		case <-tick.C:
			b.mu.Lock()
			alive := time.Now().Before(b.lease)
			active := time.Now().Before(b.pairingUntil)
			b.mu.Unlock()
			// A callback may have closed the window concurrently with this tick.
			select {
			case ip := <-b.peer:
				return vivo.Peer{Addresses: []netip.Addr{ip}}, nil
			default:
			}
			if !alive {
				return vivo.Peer{}, errors.New("Wi-Fi Direct service heartbeat lost; retry pairing")
			}
			if !active {
				return vivo.Peer{}, vivo.ErrExpired
			}
		}
	}
}

func (b *directBroker) Close() {
	b.mu.Lock()
	b.closed = true
	b.pairingUntil = time.Time{}
	callback := b.callback
	b.mu.Unlock()
	if callback != nil {
		callback.Close()
	}
	if b.api != nil {
		b.api.Close()
	}
	// No systemctl, signals, unregister request or wireless cleanup: the system
	// service and its persistent group continue running for the next frontend.
}
