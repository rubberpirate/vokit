package vivo

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileErrorCodesExcludePeerText(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{ErrFilePermission, "transfer_permission_denied"},
		{&FileOperationError{Phase: "preparation", Err: &HTTPStatusError{Status: 445}}, "preparation_http_445"},
		{&FileOperationError{Phase: "destination", Err: errors.New("phone returned an invalid upload directory")}, "destination_invalid_directory"},
		{&FileOperationError{Phase: "preparation", Err: errors.New("invalid upload response")}, "preparation_invalid_response"},
		{&FileOperationError{Phase: "preparation", Err: errors.New("peer-secret-filename-token")}, "preparation_failed"},
	} {
		if got := FileErrorCode(test.err); got != test.code {
			t.Fatal("incorrect safe error code", got)
		}
	}
}

func TestIdentityPersistsPrivatelyAndQRPreservesItsFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pairing")
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadIdentity(dir)
	if err != nil || again != id || len(id.DeviceID) != 32 {
		t.Fatalf("identity did not persist: %v", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "identity.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("identity permissions")
	}
	session, err := NewSession(id, "Laptop & Linux", "10.1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := NewSession(id, "Laptop & Linux", "10.1.2.3")
	if session.Token == other.Token || len(session.Token) != 64 {
		t.Fatal("token reused or malformed")
	}
	session.SID = "sid+/&=test"
	qr, err := session.QR("https://in-psuite.vivo.com/vbusiness")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(qr)
	if err != nil || u.Scheme != "pcsuite" || u.Opaque == "" {
		t.Fatalf("invalid export QR: %v", err)
	}
	parsed, _ := url.Parse("https://" + strings.TrimPrefix(qr, "pcsuite:"))
	q := parsed.Query()
	if q.Get("s") != "sid+/&=test" || q.Get("t") != session.Token || q.Get("f") != "pc" || q.Get("ls") != "false" || q.Get("id") != id.DeviceID {
		t.Fatal("QR lost/changed protocol fields")
	}
	if q.Get("d") != "Lap****nux" || q.Get("u") != "" {
		t.Fatal("host masking or anonymous account")
	}
	if _, err := session.QR("http://in-psuite.vivo.com/vbusiness"); err == nil {
		t.Fatal("accepted insecure cloud")
	}
	if _, err := session.QR("https://in-psuite.vivo.com/vbusiness?token=other"); err == nil {
		t.Fatal("accepted ambiguous base")
	}
	if _, err := NewSession(id, "Laptop", "not-an-ip"); err == nil {
		t.Fatal("invalid local IP")
	}
}

func TestIdentityRejectsSymlinksAndInsecureExistingFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "identity.json")
	if err := os.WriteFile(file, []byte(`{"device_id":"0123456789abcdef0123456789abcdef"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(dir); err == nil {
		t.Fatal("accepted exposed identity")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, file); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(dir); err == nil {
		t.Fatal("accepted identity symlink")
	}
	if data, _ := os.ReadFile(target); string(data) != "unchanged" {
		t.Fatal("symlink target changed")
	}
}

func TestPIDUsesRSAPKCS1v15AndIsNotThePlainDeviceID(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
	pid, err := EncryptPID(base64.StdEncoding.EncodeToString(der), "generated-device-id")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := base64.StdEncoding.DecodeString(pid)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, key, encoded)
	if err != nil || string(plain) != "generated-device-id" {
		t.Fatalf("PID interop: %v", err)
	}
	if _, err := EncryptPID("not-a-key", "id"); err == nil {
		t.Fatal("accepted invalid key")
	}
}

func TestMessageSummaryDoesNotPublishPayloads(t *testing.T) {
	for _, message := range []string{`CLIPBOARD_TEXT:private-content`, `SHADOW_LIKE:{"key":"private-key","iv":"private-iv"}`, `unknown-private-content`, `{"state":"normal"}`} {
		event := SummarizeMessage([]byte(message))
		b, _ := json.Marshal(event)
		if strings.Contains(string(b), "private-") {
			t.Fatalf("payload leaked: %s", b)
		}
	}
	event := SummarizeMessage([]byte(`PC_GET_CAMERA_CONFIG:{"secret":"hidden"}`))
	if event.Command != "PC_GET_CAMERA_CONFIG" {
		t.Fatal("lost known command")
	}
	hash := sha256.Sum256([]byte("certificate"))
	if _, err := ParseCertificatePin(hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseCertificatePin("short"); err == nil {
		t.Fatal("accepted malformed pin")
	}
}
