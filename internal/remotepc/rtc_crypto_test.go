package remotepc

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

const rtcFixtureHash = "9ccc02181dc8c6813a30c50c62398fe9358bca8835395cb85c06d4cba81a2eb1"
const rtcFixtureFingerprint = "sha-256 00:01:02:03:04:05:06:07:08:09:0A:0B:0C:0D:0E:0F:10:11:12:13:14:15:16:17:18:19:1A:1B:1C:1D:1E:1F"
const rtcFixtureEncryptedFingerprint = "R8Qwzrd77ilP/S7PkNofWlc5Le8P4ZYt163FZGaYUNGqg7nHUFfjLx+4e36g2w3DV9ApJAPR9rCNMlN+GBwXaZR0wKjPImerkFMh2aAgCIe9wKWe4WIB0+xG2rqLuCahNtLoXLmJHbI9lOKOMwhVzRlQAn2Ejp0="

func fixtureRTCCrypto(t *testing.T) *RTCCrypto {
	t.Helper()
	material := make([]byte, 96)
	for i := range material {
		material[i] = byte(i)
	}
	c, err := newRTCCrypto(rtcFixtureHash, bytes.NewReader(material))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRTCCryptoIndependentFixture(t *testing.T) {
	// Independently computed using Python hashlib.pbkdf2_hmac and AESGCM.
	if got := hex.EncodeToString(rtcPBKDF2(rtcFixtureHash, []byte{0, 1, 2, 3, 4, 5, 6, 7})); got != "3202b2983b67fa0ddc4af56133c1f5fcbef6788f180875a871ebb752a2e50ddc" {
		t.Fatal("PBKDF2 fixture mismatch")
	}
	c := fixtureRTCCrypto(t)
	info := c.SecurityInfo()
	if info.PBKDF2.Salt != "AAECAwQFBgc=" || info.PBKDF2.Iterations != 10000 || info.PBKDF2.KeyLength != 32 {
		t.Fatal("PBKDF2 wire parameters differ")
	}
	if info.AESGCM.IV != "CAkKCwwNDg8QERIT" || info.AESGCM.AAD != "FBUWFxgZGhscHR4fICEiIw==" {
		t.Fatal("AES-GCM wire parameters differ")
	}
	if info.AESGCM.MasterKey != "iUPewrlPAvt0tvH5efEuv4L5grkxxUt7JnolZ1Yd08AgmdAUy/LHJoRvZmDrUDUq5I8+4c1QsvDV2hX0N8wcNvk3IWFZXlaXjVTZ/Q==" {
		t.Fatal("wrapped master material fixture mismatch")
	}
	if info.AESGCM.VerificationMessage != "d+06qMY5r0c7qiyu5LJmPCtLS4J5rctHpf+GMRT6U43T+MaeIBruU0zdaSHn6lujUpxtUU2X+L/ZZCw6XxJqbsk3mOqGQymt7wd/g8AgZtm8wN7ItTxxx69Y1MjwlE7mKIWda9b8Dj5+Win/5TcHrCisMXpFmAjloM2+L9Txs6NcvihXwiM7AYQbPa1iaoJ+wZAQkl8KekAwiKTZ7zCyYQ==" {
		t.Fatal("verification message fixture mismatch")
	}
	plain := "v=0\r\na=fingerprint:" + rtcFixtureFingerprint + "\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=fingerprint:" + rtcFixtureFingerprint + "\n"
	want := strings.ReplaceAll(plain, rtcFixtureFingerprint, rtcFixtureEncryptedFingerprint)
	encrypted, err := c.EncryptSDP(plain)
	if err != nil || encrypted != want {
		t.Fatalf("SDP fingerprint fixture mismatch: %v", err)
	}
	decrypted, err := c.DecryptSDP(want)
	if err != nil || decrypted != plain {
		t.Fatalf("SDP decryption fixture mismatch: %v", err)
	}
}

func TestRTCCryptoShippedWindowsFixture(t *testing.T) {
	// Produced by vivoav_create_crypto_params in the shipped Windows DLL,
	// using a synthetic password hash and isolated loopback initialization.
	const passwordHash = "26750ee33fbb093a0529860d74fc8c5474f96d3633fded10ab343013d521e800e"
	const ivText = "WQr0tw9xBXdhdccz"
	const aadText = "jCKxO1Yuz5lN5n8eXWkjtw=="
	const wrappedText = "z9fkY0ooDYr/Jjh3Z5zS2gYKP1utP8S/I9ss68bT5DKJqYM0B3oM9qg1lhYyRXvil9YMcMfo/BnhhK+aoExluiGDMHj1BpCw0FiF5w=="
	const proofText = "jtIT7OXoHDTGr2mIGMKa51sRGpgFWY8QJhYrjtpL9rRYVi4Qs701UZXOaBJgmHsDSUOObCSEFt6sWunYs4psYtx4egOuzae1e6iQGbtMJuZEk019kYaWIHywUzmzx9WAqnJ0brBP/L8OHJwCZvLwYyMbVJmXKfB+ywPMTAKnKCVAshC3NXxm8/LSzx5U9uOJBVg4DpiNCosbHJJTlZno0w=="
	decode := func(text string) []byte {
		t.Helper()
		b, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	wrap, err := rtcGCM(rtcPBKDF2(passwordHash, decode("qNGWB+VSaKU=")))
	if err != nil {
		t.Fatal(err)
	}
	master, err := wrap.Open(nil, decode(ivText), decode(wrappedText), decode(aadText))
	if err != nil || len(master) != 60 {
		t.Fatalf("Windows SDK master material did not authenticate: %v", err)
	}
	session, err := rtcGCM(master[:32])
	if err != nil {
		t.Fatal(err)
	}
	proof, err := session.Open(nil, master[32:44], decode(proofText), master[44:])
	if err != nil || string(proof) != ivText+aadText+wrappedText {
		t.Fatalf("Windows SDK verification did not authenticate: %v", err)
	}
}

func TestRTCCryptoRejectsUnauthenticatedSDP(t *testing.T) {
	c := fixtureRTCCrypto(t)
	sealed, err := base64.StdEncoding.DecodeString(rtcFixtureEncryptedFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	sealed[len(sealed)-1] ^= 1
	for _, sdp := range []string{
		"v=0\r\n",
		"a=fingerprint:" + rtcFixtureFingerprint,
		"a=fingerprint:@@@\r\n",
		"a=fingerprint:" + base64.StdEncoding.EncodeToString(sealed),
		"a=fingerprint:" + c.encrypt([]byte(rtcFixtureFingerprint+"\r\na=ice-pwd:injected")),
		"a=fingerprint:" + c.encrypt([]byte("sha-256 00:11")),
	} {
		if output, err := c.DecryptSDP(sdp); err == nil || output != "" {
			t.Fatal("accepted unauthenticated or malformed fingerprint")
		}
	}
	if output, err := c.EncryptSDP("a=fingerprint:sha-256 00:11"); err == nil || output != "" {
		t.Fatal("accepted malformed plaintext fingerprint")
	}
}

func TestRTCCryptoEntropyAndHashValidation(t *testing.T) {
	for _, hash := range []string{"", "secret", strings.ToUpper(rtcFixtureHash), strings.Repeat("z", 64)} {
		if _, err := NewRTCCrypto(hash); err == nil {
			t.Fatal("accepted malformed password hash")
		}
	}
	if _, err := newRTCCrypto(rtcFixtureHash, bytes.NewReader(nil)); err == nil {
		t.Fatal("accepted failed entropy source")
	}
	first, err := NewRTCCrypto(rtcFixtureHash)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRTCCrypto(rtcFixtureHash)
	if err != nil {
		t.Fatal(err)
	}
	if first.SecurityInfo() == second.SecurityInfo() {
		t.Fatal("reused session security parameters")
	}
}
