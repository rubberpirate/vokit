package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vivolinux/internal/vivo"
)

func rememberedFixture(t *testing.T) (string, rememberedPhone) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	id, err := vivo.LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, rememberedPhone{Version: 1, PCID: id.DeviceID, PhoneIP: "192.168.49.22", LocalIP: "192.168.49.1", Token: strings.Repeat("a", 64),
		DeviceID: "phone-guid", Region: vivo.RegionChina, Direct: true, FirstUse: true}
}

func TestRememberedAuthorizationRoundTripAndDeviceIdentity(t *testing.T) {
	dir, record := rememberedFixture(t)
	if err := saveRemembered(dir, record); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(rememberedPath(dir))
	if info.Mode().Perm() != 0600 {
		t.Fatal("authorization is not private")
	}
	got, err := readRemembered(dir)
	if err != nil || got != record {
		t.Fatal("authorization did not survive restart", err)
	}
	s, _ := vivo.NewSession(vivo.Identity{DeviceID: record.PCID}, "PC", record.LocalIP)
	s.Region = record.Region
	originalConnection := s.ConnectionID
	address, err := rememberedSession(got, s)
	if err != nil || address.String() != record.PhoneIP || s.Token != record.Token || !s.AutoConnect || s.ConnectionID != originalConnection {
		t.Fatal("reconnect must keep token but use a fresh connection ID", err)
	}
	s.PhoneDeviceID = "another-guid"
	s.DeviceName = "same display name"
	if record.samePhone(s) {
		t.Fatal("accepted another device")
	}
	s.PhoneDeviceID = record.DeviceID
	if !record.samePhone(s) {
		t.Fatal("rejected the authorized device")
	}
	s.Identity.DeviceID = "another-pc"
	if _, err := rememberedSession(got, s); !errors.Is(err, errPairingRequired) {
		t.Fatal("reused a different PC's authorization")
	}
	s.Identity.DeviceID = record.PCID
	s.Region = vivo.RegionOverseas
	if _, err := rememberedSession(got, s); !errors.Is(err, errPairingRequired) {
		t.Fatal("reused authorization across regions")
	}
	if err := forgetRemembered(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := readRemembered(dir); !errors.Is(err, errPairingRequired) {
		t.Fatal("manual disconnect retained authorization")
	}
}

func TestRememberedRejectsUnsafeStateWithoutRevealingSecrets(t *testing.T) {
	dir, r := rememberedFixture(t)
	for _, change := range []func(*rememberedPhone){
		func(r *rememberedPhone) { r.PhoneIP = "8.8.8.8" }, func(r *rememberedPhone) { r.PhoneIP = r.LocalIP },
		func(r *rememberedPhone) { r.Token = "secret" }, func(r *rememberedPhone) { r.DeviceID = "" },
		func(r *rememberedPhone) { r.Region = "unknown" }, func(r *rememberedPhone) { r.FirstUse = false },
	} {
		bad := r
		change(&bad)
		if bad.valid() {
			t.Fatal("accepted unsafe remembered record")
		}
	}
	data, _ := json.Marshal(r)
	path := rememberedPath(dir)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readRemembered(dir); err == nil || strings.Contains(err.Error(), r.Token) {
		t.Fatal("public credentials accepted or leaked")
	}
	os.Remove(path)
	target := filepath.Join(t.TempDir(), "secret.json")
	os.WriteFile(target, data, 0600)
	os.Symlink(target, path)
	if _, err := readRemembered(dir); err == nil {
		t.Fatal("followed an authorization symlink")
	}
}

func TestAuthorizationErrorsRequireFreshPairing(t *testing.T) {
	for _, err := range []error{vivo.ErrPhoneAuthorization, &vivo.HTTPStatusError{Status: 401}, &vivo.HTTPStatusError{Status: 403}} {
		if !vivo.NeedsPairing(err) {
			t.Fatal("authorization failure would be retried", err)
		}
	}
	if vivo.NeedsPairing(&vivo.HTTPStatusError{Status: 500}) {
		t.Fatal("transient server error disabled reconnect")
	}
}
