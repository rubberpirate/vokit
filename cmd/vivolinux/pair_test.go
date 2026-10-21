package main

import (
	"context"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPairRejectsInvalidOptionsBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"--phone", "8.8.8.8"}, {"--phone-cert-sha256", "invalid"},
		{"--duration", "0s"}, {"--duration", "25h"}, {"unexpected"},
		{"--local-ip", "not-an-ip"}, {"--phone", "localhost"},
		{"--phone", "::ffff:10.1.2.3%wlan0"},
		{"--trust-scanned-phone"},
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
	options, err = parsePairOptions([]string{"--phone", "10.1.2.3", "--trust-scanned-phone"})
	if err != nil || !options.trustScanned {
		t.Fatal("explicit first-use policy rejected", err)
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
