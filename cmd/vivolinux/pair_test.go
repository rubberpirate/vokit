package main

import (
	"context"
	"errors"
	"image/png"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vivolinux/internal/vivo"
)

func TestPairRejectsInvalidOptionsBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"--phone", "8.8.8.8"}, {"--phone-cert-sha256", "invalid"},
		{"--duration", "0s"}, {"--duration", "25h"}, {"unexpected"},
		{"--local-ip", "not-an-ip"}, {"--phone", "localhost"},
		{"--phone", "::ffff:10.1.2.3%wlan0"},
		{"--receive-dir", "relative"}, {"--receive-dir", "/"}, {"--receive-dir", "/tmp/../unsafe"},
		{"--phone", "10.1.2.3", "--trust-scanned-phone", "--phone-cert-sha256", strings.Repeat("a", 64)},
	} {
		if _, err := parsePairOptions(args); err == nil {
			t.Fatalf("accepted invalid options %q", args)
		}
	}
	options, err := parsePairOptions([]string{"--phone", "10.1.2.3", "--local-ip", "10.1.2.1"})
	if err != nil || options.phone.String() != "10.1.2.3" {
		t.Fatalf("valid options rejected: %v", err)
	}
	options, err = parsePairOptions([]string{"--trust-scanned-phone"})
	if err != nil || !options.trustScanned || options.phone.IsValid() {
		t.Fatal("explicit first-use policy rejected", err)
	}
}

func TestPairFailureCodesAreBoundedAndSecretFree(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"cancelled", context.Canceled, "cancelled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"phone listener not ready", vivo.ErrPhoneNotListening, "phone_not_listening"},
		{"network timeout", &net.DNSError{Err: "private-name.example", IsTimeout: true}, "timeout"},
		{"TLS failure", errors.New("phone TLS/network request failed; secret=never-log"), "network_or_tls"},
		{"TLS failure mentions certificate pin", errors.New("phone TLS/network request failed; verify its certificate pin and reachability"), "network_or_tls"},
		{"rendezvous TLS", errors.New("scan result: rendezvous network/TLS request failed"), "network_or_tls"},
		{"pin mismatch", errors.New("phone certificate changed; pinned certificate mismatch"), "certificate_trust"},
		{"rejection", errors.New("phone rejected version negotiation"), "phone_rejected_or_incompatible"},
		{"expiry", errors.New("scan session expired"), "expired"},
		{"empty candidates", errors.New("no reachable phone was returned by this QR scan"), "no_usable_lan_endpoint"},
		{"protocol", errors.New("invalid response"), "protocol_or_response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pairFailureCode(test.err); got != test.want {
				t.Fatalf("pairFailureCode() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestQRIsPrivatePNGAndDoesNotOverwrite(t *testing.T) {
	if _, err := exec.LookPath("qrencode"); err != nil {
		t.Skip("qrencode not installed")
	}
	path := filepath.Join(t.TempDir(), "pair.png")
	if err := writeQR(context.Background(), "pcsuite:example.invalid?secret=test", path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("QR permissions: %v %v", info, err)
	}
	f, _ := os.Open(path)
	defer f.Close()
	if _, err := png.DecodeConfig(f); err != nil {
		t.Fatal("not a valid PNG", err)
	}
	if err := writeQR(context.Background(), "replacement", path); err == nil {
		t.Fatal("overwrote an existing private QR")
	}
	link := filepath.Join(t.TempDir(), "symlink.png")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeQR(context.Background(), "replacement", link); err == nil {
		t.Fatal("followed QR symlink")
	}
}

func TestNativeCommandsAdvertised(t *testing.T) {
	if !strings.Contains(usage, "vivolinux pair") || !strings.Contains(usage, "phone-certificate") {
		t.Fatal("native pairing commands missing from help")
	}
}
