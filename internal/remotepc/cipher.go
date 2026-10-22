package remotepc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"hash/crc32"
	"math/big"
	"strings"
	"unicode/utf8"
)

// Cipher implements the recovered Maodun HTTP envelope, independently of RTC keying.
// Each client retains its own AES key because server replies use that request key.
type Cipher struct {
	key, iv [16]byte
	header  []byte
}

func NewCipher() (*Cipher, error) {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	c := &Cipher{}
	for _, dst := range [][]byte{c.key[:], c.iv[:]} {
		for i := range dst {
			n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return nil, errors.New("remote HTTP cipher randomness failed")
			}
			dst[i] = alphabet[n.Int64()]
		}
	}
	protocol, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(publicProtocolHeader, "="))
	if err != nil || len(protocol) < 10 || binary.BigEndian.Uint16(protocol) != 204 {
		return nil, errors.New("invalid remote HTTP public header")
	}
	tokenLen := int(protocol[5])
	if len(protocol) <= tokenLen+10 {
		return nil, errors.New("invalid remote HTTP public key")
	}
	block, _ := pem.Decode(protocol[tokenLen+10:])
	if block == nil {
		return nil, errors.New("invalid remote HTTP public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid remote HTTP public key")
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("invalid remote HTTP public key type")
	}
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, key, c.key[:])
	if err != nil {
		return nil, errors.New("remote HTTP key wrapping failed")
	}
	b := append([]byte{}, protocol[tokenLen+6:tokenLen+8]...)
	b = binary.BigEndian.AppendUint16(b, 203)
	b = append(b, protocol[tokenLen+8:tokenLen+10]...)
	b = append(b, protocol[4:tokenLen+6]...)
	b = append(b, c.iv[:]...)
	b = binary.BigEndian.AppendUint16(b, uint16(len(wrapped)))
	b = append(b, wrapped...)
	c.header = binary.BigEndian.AppendUint16(nil, uint16(len(b)+10))
	c.header = binary.BigEndian.AppendUint64(c.header, uint64(crc32.ChecksumIEEE(b)))
	c.header = append(c.header, b...)
	return c, nil
}

func (c *Cipher) Encrypt(plain string) string {
	data := []byte(plain)
	padding := aes.BlockSize - len(data)%aes.BlockSize
	for i := 0; i < padding; i++ {
		data = append(data, byte(padding))
	}
	block, _ := aes.NewCipher(c.key[:])
	cipher.NewCBCEncrypter(block, c.iv[:]).CryptBlocks(data, data)
	frame := append(append([]byte{}, c.header...), data...)
	return base64.RawURLEncoding.EncodeToString(frame)
}

func (c *Cipher) Decrypt(encoded string) (string, error) {
	if len(encoded) > 1024*1024 {
		return "", errors.New("remote HTTP ciphertext too large")
	}
	// The SDK sends URL-safe base64; production replies also use standard base64.
	encoded = strings.NewReplacer("-", "+", "_", "/").Replace(encoded)
	encoding := base64.RawStdEncoding
	if strings.HasSuffix(encoded, "=") {
		encoding = base64.StdEncoding
	}
	frame, err := encoding.DecodeString(encoded)
	if err != nil {
		return "", errors.New("invalid remote HTTP ciphertext encoding")
	}
	if len(frame) < 36 || binary.BigEndian.Uint16(frame[12:14]) != 203 {
		return "", errors.New("invalid remote HTTP ciphertext header")
	}
	tokenLen := int(frame[17])
	keyLenOffset := tokenLen + 34
	if keyLenOffset+2 > len(frame) {
		return "", errors.New("truncated remote HTTP ciphertext header")
	}
	start := keyLenOffset + 2 + int(binary.BigEndian.Uint16(frame[keyLenOffset:keyLenOffset+2]))
	if start > len(frame) || int(binary.BigEndian.Uint16(frame[:2])) != start {
		return "", errors.New("invalid remote HTTP ciphertext length")
	}
	if binary.BigEndian.Uint64(frame[2:10]) != uint64(crc32.ChecksumIEEE(frame[10:start])) {
		return "", errors.New("invalid remote HTTP ciphertext checksum")
	}
	data := frame[start:]
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return "", errors.New("invalid remote HTTP ciphertext block length")
	}
	block, _ := aes.NewCipher(c.key[:])
	cipher.NewCBCDecrypter(block, c.iv[:]).CryptBlocks(data, data)
	padding := int(data[len(data)-1])
	if padding == 0 || padding > aes.BlockSize || padding > len(data) {
		return "", errors.New("invalid remote HTTP ciphertext padding")
	}
	for _, b := range data[len(data)-padding:] {
		if int(b) != padding {
			return "", errors.New("invalid remote HTTP ciphertext padding")
		}
	}
	plain := data[:len(data)-padding]
	if !utf8.Valid(plain) {
		return "", errors.New("invalid remote HTTP plaintext encoding")
	}
	return string(plain), nil
}
