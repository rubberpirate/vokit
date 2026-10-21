// Package vivo implements the experimental, explicitly paired LAN control path.
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
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Public rendezvous encryption key from app-connection.js:377830, production branch.
// This encrypts a generated PC identifier, not the phone TLS connection.
const rendezvousPublicKey = "MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDD/1z/ZaMFv7uz1kvqGjJpw8B8tHWukJIey5Y4RRpBiBesxPhcfXFqaYA3OnamQAoJnCY+7Vrmk6j6x8px7Tq4iF8Ebim77dfFEpqvfJaVXdg8gzIVoGO/5L6ucSx9NDEl7T3CiFOCy4mPlTQ4nOksidxvLi9mpNAaKSdvtGw01wIDAQAB"

type Identity struct {
	DeviceID string `json:"device_id"`
}

type HTTPStatusError struct{ Status int }

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("HTTP status %d", e.Status) }

type FileOperationError struct {
	Phase string
	Err   error
}

func (e *FileOperationError) Error() string { return "file operation failed during " + e.Phase }
func (e *FileOperationError) Unwrap() error { return e.Err }
func FileErrorCode(err error) string {
	phase := "transfer"
	var operation *FileOperationError
	if errors.As(err, &operation) {
		phase = operation.Phase
	}
	if errors.Is(err, ErrFilePermission) {
		return phase + "_permission_denied"
	}
	var status *HTTPStatusError
	if errors.As(err, &status) {
		return fmt.Sprintf("%s_http_%d", phase, status.Status)
	}
	if operation != nil {
		// Only fixed local messages become diagnostic codes; peer text stays private.
		switch operation.Err.Error() {
		case "phone returned an invalid upload directory":
			return phase + "_invalid_directory"
		case "invalid JSON response", "invalid upload response", "invalid or oversized upload response":
			return phase + "_invalid_response"
		case "phone rejected upload":
			return phase + "_rejected"
		case "upload preparation failed", "phone TLS/network request failed; verify its certificate pin and reachability":
			return phase + "_network_failed"
		}
	}
	return phase + "_failed"
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func privateFile(info os.FileInfo, mode os.FileMode) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && s.Uid == uint32(os.Getuid()) && info.Mode().Perm()&0077 == 0 && info.Mode()&os.ModeSymlink == 0 && info.Mode().Type() == mode
}

func LoadIdentity(dir string) (Identity, error) {
	var id Identity
	if err := os.MkdirAll(dir, 0700); err != nil {
		return id, errors.New("cannot create pairing state directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !privateFile(info, os.ModeDir) {
		return id, errors.New("pairing state directory must be private, user-owned, and not a symlink")
	}
	path := filepath.Join(dir, "identity.json")
	if info, err := os.Lstat(path); err == nil {
		if !privateFile(info, 0) {
			return id, errors.New("identity file must be private, user-owned, regular, and not a symlink")
		}
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return id, errors.New("cannot open identity file")
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil || len(data) > 4096 || json.Unmarshal(data, &id) != nil {
			return id, errors.New("invalid identity file")
		}
		b, err := hex.DecodeString(id.DeviceID)
		if err != nil || len(b) != 16 {
			return id, errors.New("invalid device identity")
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return id, errors.New("cannot inspect identity file")
	}
	id.DeviceID, err = randomHex(16)
	if err != nil {
		return id, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return Identity{}, errors.New("cannot create identity file; another pairing process may be running")
	}
	data, _ := json.Marshal(id)
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return Identity{}, errors.New("cannot persist identity")
	}
	return id, closeErr
}

type Session struct {
	Identity     Identity
	HostName     string
	Token        string
	SID          string
	ConnectionID string
	StorageRoot  string `json:"-"`
	DeviceName   string `json:"-"`
	ExpiresAt    time.Time
}

func NewSession(identity Identity, hostname, localIP string) (*Session, error) {
	if identity.DeviceID == "" {
		return nil, errors.New("missing PC identity")
	}
	if _, err := netip.ParseAddr(localIP); err != nil {
		return nil, errors.New("invalid local IP")
	}
	if hostname == "" || len(hostname) > 200 || strings.ContainsFunc(hostname, unicode.IsControl) {
		return nil, errors.New("invalid PC name")
	}
	random, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(localIP + random))
	connection, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	return &Session{Identity: identity, HostName: hostname, Token: hex.EncodeToString(digest[:]), ConnectionID: fmt.Sprintf("%s_%d", connection, time.Now().UnixMilli())}, nil
}

func (s *Session) QR(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("QR base must be a plain HTTPS origin/path")
	}
	if s.SID == "" || s.Token == "" {
		return "", errors.New("missing scan session")
	}
	host := []rune(s.HostName)
	name := s.HostName
	if len(host) > 6 {
		name = string(host[:3]) + "****" + string(host[len(host)-3:])
	}
	q := url.Values{"s": {s.SID}, "f": {"pc"}, "t": {s.Token}, "u": {""}, "d": {name}, "mac": {""}, "ls": {"false"}, "pb": {"0"}, "id": {s.Identity.DeviceID}}
	u.RawQuery = q.Encode()
	return "pcsuite:" + strings.TrimPrefix(u.String(), "https://"), nil
}

func EncryptPID(encodedKey, deviceID string) (string, error) {
	der, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil {
		return "", errors.New("invalid rendezvous public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return "", errors.New("invalid rendezvous public key")
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("rendezvous key is not RSA")
	}
	data, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(deviceID))
	if err != nil {
		return "", errors.New("cannot encrypt rendezvous identity")
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func ParseCertificatePin(s string) ([]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("phone certificate pin must be 64 hexadecimal characters")
	}
	return b, nil
}

type Event struct {
	Time         time.Time       `json:"time"`
	Stage        string          `json:"stage"`
	Command      string          `json:"command,omitempty"`
	Count        int             `json:"count,omitempty"`
	Code         string          `json:"code,omitempty"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
	QRPath       string          `json:"qr_path,omitempty"`
	MediaTiming  *MediaTiming    `json:"media_timing,omitempty"`
}

func SummarizeMessage(data []byte) Event {
	e := Event{Time: time.Now().UTC(), Stage: "control_message", Command: "unknown"}
	if string(data) == `{"state":"normal"}` {
		e.Command = "heartbeat"
		return e
	}
	for _, command := range []string{"SHARE_TRANS_FILE", "FILE_TRANS_TAG", "TRANS_FILE_FAIL", "TRANS_FILE_CANCEL", "UPDATE_DEVICE_INFO", "UPDATE_STORAGE", "UPDATE_PHONE_DIMENSIONS", "IS_CROSS_NETWORK", "PC_GET_CAMERA_CONFIG", "PC_GET_CAMERA_CAPABILITY", "PC_GET_CAMERA_STATUS", "FunctionSupported", "SHADOW_LIKE", "FILE_MANAGER_SWITCH", "RE_CONNECT_ALBUM", "CLIPBOARD_TEXT", "RELAY_CONNECT_STATUS", "PHONE_DEVICEID", "TELECOM_INFO", "PC_RESPONSE_CAMERA_AUTH", "PC_THIRD_PARTY_APP"} {
		if strings.HasPrefix(string(data), command+":") {
			e.Command = command
			return e
		}
	}
	return e
}
