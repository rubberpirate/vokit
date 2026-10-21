package vivo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDefaultRendezvousPreservesTLSAndDoesNotMutateSharedClient(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":{"sid":"fixture","timeout":180000}}`)
	}))
	defer server.Close()
	original := http.DefaultClient
	http.DefaultClient = server.Client()
	defer func() { http.DefaultClient = original }()
	shared := http.DefaultClient
	cloud, err := NewRendezvous(server.URL, Identity{DeviceID: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cloud.http.CloseIdleConnections()
	if cloud.http.Transport == shared.Transport || shared.CheckRedirect != nil {
		t.Fatal("rendezvous modified shared HTTP configuration")
	}
	session := &Session{}
	if err := cloud.Begin(context.Background(), session); err != nil || session.SID != "fixture" {
		t.Fatal("default client TLS configuration was not preserved", err)
	}
}

func TestRendezvousObtainsExpiringSIDAndDecodesNestedPhoneReply(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("pid") == "" || r.Header.Get("pid") == "0123456789abcdef0123456789abcdef" {
			t.Error("missing/encrypted PID")
		}
		switch r.URL.Path {
		case "/vbusiness/scan/sid":
			if r.Method != "GET" {
				t.Error("SID method")
			}
			fmt.Fprint(w, `{"code":0,"msg":"ok","data":{"sid":"fresh-sid","device":"unused","timeout":180000}}`)
		case "/vbusiness/scan/getPhone":
			if r.Method != "POST" || r.Header.Get("userId") != "71f6033b2caa9784" {
				t.Error("phone rendezvous method/headers")
			}
			var request map[string]string
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["sid"] != "fresh-sid" {
				t.Error("scan SID not sent")
			}
			fmt.Fprint(w, `{"code":0,"data":"{\"ip\":[\"10.1.2.3\",\"fd00::2\"],\"connectionId\":\"fresh-connection\",\"deviceName\":\"Private Name\"}"}`)
		default:
			t.Error("unexpected request")
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cloud, err := NewRendezvous(server.URL+"/vbusiness", Identity{DeviceID: "0123456789abcdef0123456789abcdef"}, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	s, _ := NewSession(Identity{DeviceID: "0123456789abcdef0123456789abcdef"}, "Linux laptop", "10.1.1.1")
	if err := cloud.Begin(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.SID != "fresh-sid" || time.Until(s.ExpiresAt) < 170*time.Second {
		t.Fatal("SID expiry missing")
	}
	peer, err := cloud.Wait(context.Background(), s)
	if err != nil || len(peer.Addresses) != 2 || peer.Addresses[0].String() != "10.1.2.3" || peer.ConnectionID != "fresh-connection" {
		t.Fatalf("phone reply: %#v %v", peer, err)
	}
}

func TestRendezvousRejectsExpiryPublicEndpointsAndOversizedReplies(t *testing.T) {
	for _, fixture := range []struct {
		name, reply string
		expired     bool
	}{
		{"expired", `{"code":10003,"data":null}`, true},
		{"public", `{"code":0,"data":"{\"ip\":[\"8.8.8.8\"]}"}`, false},
		{"loopback", `{"code":0,"data":"{\"ip\":[\"127.0.0.1\"]}"}`, false},
		{"bad-json", `{"code":0,"data":"not-json-secret"}`, false},
		{"too-large", strings.Repeat("x", 600*1024), false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, fixture.reply) }))
			defer server.Close()
			cloud, _ := NewRendezvous(server.URL+"/vbusiness", Identity{DeviceID: "pc"}, server.Client())
			_, err := cloud.Wait(context.Background(), &Session{SID: "secret-sid", ExpiresAt: time.Now().Add(time.Minute)})
			if err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("bad/unsafe error: %v", err)
			}
			if fixture.expired && !errors.Is(err, ErrExpired) {
				t.Fatalf("expiry not recognized: %v", err)
			}
		})
	}
}

func TestRendezvousKeepsLANPeerWhenOtherInterfacesAreAdvertised(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":0,"data":"{\"ip\":[\"2001:db8::1\",\"10.1.2.3\",\"fe80::1%wlan0\",\"127.0.0.1\",\"not-an-address-secret\",\"::ffff:10.1.2.3%wlan0\"]}"}`)
	}))
	defer server.Close()
	cloud, _ := NewRendezvous(server.URL, Identity{DeviceID: "pc"}, server.Client())
	peer, err := cloud.Wait(context.Background(), &Session{SID: "s", ExpiresAt: time.Now().Add(time.Minute)})
	if err != nil || len(peer.Addresses) != 1 || peer.Addresses[0].String() != "10.1.2.3" || peer.IgnoredAddresses != 5 {
		t.Fatalf("safe candidate lost or unsafe candidate accepted: %#v %v", peer, err)
	}
}

func TestRendezvousDoesNotFollowRedirectOrStartExpiredPoll(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "https://example.invalid/secret", 307)
	}))
	defer server.Close()
	cloud, _ := NewRendezvous(server.URL+"/vbusiness", Identity{DeviceID: "pc"}, server.Client())
	if err := cloud.Begin(context.Background(), &Session{}); err == nil {
		t.Fatal("accepted redirect")
	}
	if _, err := cloud.Wait(context.Background(), &Session{SID: "s", ExpiresAt: time.Now().Add(-time.Second)}); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatal("expired poll contacted endpoint")
	}
}
