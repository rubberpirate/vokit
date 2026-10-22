package remotepc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// RTCSecurityInfo is the native SDK's openSuccess.p2pSecurityInfo payload.
type RTCSecurityInfo struct {
	PBKDF2 struct {
		Salt       string `json:"salt"`
		Iterations int    `json:"iterations"`
		KeyLength  int    `json:"keyLength"`
	} `json:"pbkdf2"`
	AESGCM struct {
		IV                  string `json:"iv"`
		AAD                 string `json:"aad"`
		MasterKey           string `json:"masterKey"`
		VerificationMessage string `json:"verificationMessage"`
	} `json:"aesGcm"`
}

// RTCCrypto wraps the negotiated DTLS fingerprints, not the RTP media stream.
// The protocol reuses its session IV; preserve it for interoperability.
type RTCCrypto struct {
	info RTCSecurityInfo
	aead cipher.AEAD
	iv   []byte
	aad  []byte
}

// NewRTCCrypto takes the lowercase SHA256 hex digest of password + "salt".
// Recovered from the symbolized macOS vivo_rtc.dylib, SHA256 6858893c3ff07fb7…,
// vivoav_create_crypto_params (0x67870) and Conductor::EncryptSdp (0x102d0).
func NewRTCCrypto(passwordHash string) (*RTCCrypto, error) {
	return newRTCCrypto(passwordHash, rand.Reader)
}

func newRTCCrypto(passwordHash string, entropy io.Reader) (*RTCCrypto, error) {
	decoded, err := hex.DecodeString(passwordHash)
	if err != nil || len(decoded) != sha256.Size || passwordHash != strings.ToLower(passwordHash) {
		return nil, errors.New("RTC password hash must be a lowercase SHA256 hex digest")
	}
	material := make([]byte, 8+12+16+60)
	if _, err := io.ReadFull(entropy, material); err != nil {
		return nil, errors.New("could not generate RTC security parameters")
	}
	salt, iv, aad, master := material[:8], material[8:20], material[20:36], material[36:]
	passwordKey := rtcPBKDF2(passwordHash, salt)
	wrapAEAD, err := rtcGCM(passwordKey)
	if err != nil {
		return nil, err
	}
	c := &RTCCrypto{iv: master[32:44], aad: master[44:]}
	c.aead, err = rtcGCM(master[:32])
	if err != nil {
		return nil, err
	}
	c.info.PBKDF2.Salt = base64.StdEncoding.EncodeToString(salt)
	c.info.PBKDF2.Iterations = 10000
	c.info.PBKDF2.KeyLength = 32
	c.info.AESGCM.IV = base64.StdEncoding.EncodeToString(iv)
	c.info.AESGCM.AAD = base64.StdEncoding.EncodeToString(aad)
	c.info.AESGCM.MasterKey = base64.StdEncoding.EncodeToString(wrapAEAD.Seal(nil, iv, master, aad))
	verification := c.info.AESGCM.IV + c.info.AESGCM.AAD + c.info.AESGCM.MasterKey
	c.info.AESGCM.VerificationMessage = c.encrypt([]byte(verification))
	return c, nil
}

func rtcGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("invalid RTC encryption key")
	}
	return cipher.NewGCM(block)
}

// The SDK requests exactly one SHA256-sized PBKDF2 block.
func rtcPBKDF2(password string, salt []byte) []byte {
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write(salt)
	mac.Write([]byte{0, 0, 0, 1})
	u := mac.Sum(nil)
	key := append([]byte(nil), u...)
	for i := 1; i < 10000; i++ {
		mac.Reset()
		mac.Write(u)
		u = mac.Sum(u[:0])
		for j := range key {
			key[j] ^= u[j]
		}
	}
	return key
}

func (c *RTCCrypto) SecurityInfo() RTCSecurityInfo { return c.info }

func (c *RTCCrypto) encrypt(plaintext []byte) string {
	return base64.StdEncoding.EncodeToString(c.aead.Seal(nil, c.iv, plaintext, c.aad))
}

func (c *RTCCrypto) EncryptSDP(sdp string) (string, error) {
	return c.transformSDP(sdp, false)
}

// DecryptSDP requires authenticated fingerprints when encryption is negotiated.
func (c *RTCCrypto) DecryptSDP(sdp string) (string, error) {
	return c.transformSDP(sdp, true)
}

func (c *RTCCrypto) transformSDP(sdp string, decrypt bool) (string, error) {
	if c == nil || c.aead == nil || len(sdp) == 0 || len(sdp) > 1024*1024 {
		return "", errors.New("invalid encrypted RTC SDP")
	}
	lines := strings.SplitAfter(sdp, "\n")
	count := 0
	for i, line := range lines {
		body := strings.TrimSuffix(line, "\n")
		body = strings.TrimSuffix(body, "\r")
		if !strings.HasPrefix(body, "a=fingerprint:") {
			continue
		}
		value := strings.TrimPrefix(body, "a=fingerprint:")
		if decrypt {
			sealed, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				return "", errors.New("invalid encrypted RTC fingerprint")
			}
			plaintext, err := c.aead.Open(nil, c.iv, sealed, c.aad)
			if err != nil {
				return "", errors.New("RTC fingerprint authentication failed")
			}
			value = string(plaintext)
		}
		if !validRTCFingerprint(value) {
			return "", errors.New("invalid RTC SHA256 fingerprint")
		}
		if !decrypt {
			value = c.encrypt([]byte(value))
		}
		lines[i] = "a=fingerprint:" + value + line[len(body):]
		count++
	}
	if count == 0 {
		return "", errors.New("RTC SDP has no fingerprint")
	}
	return strings.Join(lines, ""), nil
}

func validRTCFingerprint(value string) bool {
	if !strings.HasPrefix(value, "sha-256 ") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "sha-256 "), ":")
	if len(parts) != sha256.Size {
		return false
	}
	for _, part := range parts {
		if len(part) != 2 {
			return false
		}
		if _, err := hex.DecodeString(part); err != nil {
			return false
		}
	}
	return true
}
