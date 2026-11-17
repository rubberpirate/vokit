package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOfficialLoginCallbackAndPrivateAccountStorage(t *testing.T) {
	a, err := parseLoginPayload("account&secret-token&login-device&CN&&cloud&csrf")
	if err != nil || a.DeviceID != "login-device" {
		t.Fatal(err)
	}
	for _, payload := range []string{"", "null&secret&device&CN", "account&&device&CN", "account|injected&secret&device&CN", strings.Repeat("x", 65537)} {
		if _, err := parseLoginPayload(payload); err == nil {
			t.Fatal("invalid login callback accepted")
		}
	}
	if !isAccountCallback(loginCallback + "?openid=fixture") {
		t.Fatal("official callback rejected")
	}
	for _, u := range []string{"http://psuite.vivo.com.cn/vbusiness/account/cookie/getHtml?openid=x", loginCallback, "https://psuite.vivo.com.cn.evil.invalid/vbusiness/account/cookie/getHtml?openid=x", "https://user@psuite.vivo.com.cn/vbusiness/account/cookie/getHtml?openid=x"} {
		if isAccountCallback(u) {
			t.Fatal("untrusted login callback accepted")
		}
	}
	dir := filepath.Join(t.TempDir(), "account-state")
	if err := savePrivateState(dir, "account.json", a); err != nil {
		t.Fatal(err)
	}
	got, err := loadAccount(dir)
	if err != nil || got != a {
		t.Fatal("account did not survive restart", err)
	}
	info, _ := os.Stat(filepath.Join(dir, "account.json"))
	if info.Mode().Perm() != 0600 {
		t.Fatal("account credentials are not private")
	}
	os.Chmod(filepath.Join(dir, "account.json"), 0644)
	if _, err := loadAccount(dir); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("public credentials accepted or logged")
	}
}
