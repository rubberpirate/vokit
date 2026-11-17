package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"vivolinux/internal/vivo"
)

// One last explicitly authorized phone. Never use a name as device identity.
// Tokens are private state, never events, arguments or diagnostics.
type rememberedPhone struct {
	Version    int             `json:"version"`
	PCID       string          `json:"pc_id"`
	PhoneIP    string          `json:"phone_ip"`
	LocalIP    string          `json:"local_ip"`
	Token      string          `json:"token"`
	DeviceID   string          `json:"device_id"`
	BleID      string          `json:"ble_id"`
	Region     vivo.Region     `json:"region"`
	Direct     bool            `json:"wifi_direct"`
	FirstUse   bool            `json:"first_use"`
	Pin        string          `json:"pin"`
	DeviceName string          `json:"device_name"`
	Preconnect *preconnectPeer `json:"preconnect,omitempty"`
}

var errPairingRequired = errors.New("saved phone authorization is unavailable or changed; scan a fresh QR")

func rememberedPath(dir string) string { return filepath.Join(dir, "remembered.json") }

func (r rememberedPhone) valid() bool {
	if r.Preconnect != nil && !r.Preconnect.valid() {
		return false
	}
	if len(r.DeviceName) > 256 || strings.ContainsAny(r.DeviceName, "\x00\r\n") {
		return false
	}
	_, regionErr := vivo.ParseRegion(string(r.Region))
	ip, e1 := privateAddress(r.PhoneIP)
	local, e2 := privateAddress(r.LocalIP)
	token, e3 := hex.DecodeString(r.Token)
	identity := func(value string) bool { return len(value) <= 512 && !strings.ContainsAny(value, "\x00\r\n") }
	_, pinErr := vivo.ParseCertificatePin(r.Pin)
	return r.Version == 1 && r.PCID != "" && e1 == nil && e2 == nil && ip.Is4() && local.Is4() && ip != local && e3 == nil && len(token) == 32 && regionErr == nil &&
		identity(r.DeviceID) && identity(r.BleID) && (r.DeviceID != "" || r.BleID != "") && ((r.FirstUse && r.Pin == "") || (!r.FirstUse && pinErr == nil))
}

func readRemembered(dir string) (rememberedPhone, error) {
	var r rememberedPhone
	f, err := os.OpenFile(rememberedPath(dir), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return r, errPairingRequired
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return r, errPairingRequired
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return r, errPairingRequired
	}
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 || json.Unmarshal(data, &r) != nil || !r.valid() {
		return r, errPairingRequired
	}
	return r, nil
}

func saveRemembered(dir string, r rememberedPhone) error {
	if !r.valid() {
		return errPairingRequired
	}
	f, err := os.CreateTemp(dir, ".remembered-")
	if err != nil {
		return errors.New("cannot save phone authorization")
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(r); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), rememberedPath(dir))
	}
	if err != nil {
		return errors.New("cannot save phone authorization")
	}
	return nil
}

func forgetRemembered(dir string) error {
	// Validate the private state directory before any removal, including symlinks.
	if _, err := vivo.LoadIdentity(dir); err != nil {
		return err
	}
	if err := os.Remove(rememberedPath(dir)); err != nil && !os.IsNotExist(err) {
		return errors.New("cannot forget saved phone authorization")
	}
	return nil
}

func (r rememberedPhone) samePhone(s *vivo.Session) bool {
	if r.DeviceID != "" {
		return s.PhoneDeviceID == r.DeviceID
	}
	return r.BleID != "" && strings.EqualFold(r.BleID, s.PhoneBleID)
}

func rememberedSession(r rememberedPhone, s *vivo.Session) (netip.Addr, error) {
	if !r.valid() || r.PCID != s.Identity.DeviceID || r.Region != s.Region {
		return netip.Addr{}, errPairingRequired
	}
	s.Token = r.Token
	s.AutoConnect = true
	return netip.ParseAddr(r.PhoneIP)
}

func rememberedLANAvailable(r rememberedPhone) bool {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	phone, _ := netip.ParseAddr(r.PhoneIP)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || strings.HasPrefix(iface.Name, "p2p-") {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, a := range addresses {
			p, err := netip.ParsePrefix(a.String())
			if err == nil && p.Addr().String() == r.LocalIP && p.Contains(phone) {
				return true
			}
		}
	}
	return false
}
