package vivo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrExpired = errors.New("scan session expired; generate a fresh QR")

const IndiaGateway = "https://in-psuite.vivo.com/vbusiness"
const maxResponseBytes = 512 * 1024

type Rendezvous struct {
	base string
	pid  string
	http *http.Client
}
type Peer struct {
	Addresses        []netip.Addr
	ConnectionID     string
	IgnoredAddresses int
}

func NewRendezvous(base string, identity Identity, client *http.Client) (*Rendezvous, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid HTTPS rendezvous base")
	}
	pid, err := EncryptPID(rendezvousPublicKey, identity.DeviceID)
	if err != nil {
		return nil, err
	}
	if client == nil {
		configured := *http.DefaultClient
		transport := configured.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		if standard, ok := transport.(*http.Transport); ok {
			bounded := standard.Clone()
			// Finish address fallback before the ten-second QR bootstrap expires.
			bounded.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
			configured.Transport = bounded
		}
		client = &configured
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Rendezvous{base: strings.TrimSuffix(base, "/"), pid: pid, http: &copy}, nil
}

// readJSON never includes remote response bodies or URL query values in errors.
func readJSON(response *http.Response, target any) error {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &HTTPStatusError{Status: response.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("cannot read response")
	}
	if len(b) > maxResponseBytes {
		return errors.New("response exceeds size limit")
	}
	if json.Unmarshal(b, target) != nil {
		return errors.New("invalid JSON response")
	}
	return nil
}

func (c *Rendezvous) request(ctx context.Context, method, path string, body any, target any) error {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("cannot encode request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("cannot build rendezvous request")
	}
	req.Header.Set("pid", c.pid)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("userId", "71f6033b2caa9784")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("rendezvous network/TLS request failed")
	}
	return readJSON(response, target)
}

func (c *Rendezvous) Begin(ctx context.Context, s *Session) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var reply struct {
		Code *int `json:"code"`
		Data struct {
			SID     string `json:"sid"`
			Timeout int64  `json:"timeout"`
		} `json:"data"`
	}
	if err := c.request(ctx, "GET", "/scan/sid", nil, &reply); err != nil {
		return fmt.Errorf("scan bootstrap: %w", err)
	}
	if reply.Code == nil || *reply.Code != 0 {
		return errors.New("scan bootstrap rejected by server")
	}
	if reply.Data.SID == "" || len(reply.Data.SID) > 4096 || reply.Data.Timeout < 1000 || reply.Data.Timeout > 10*60*1000 {
		return errors.New("invalid scan session/expiry")
	}
	s.SID = reply.Data.SID
	s.ExpiresAt = time.Now().Add(time.Duration(reply.Data.Timeout) * time.Millisecond)
	return nil
}

func (c *Rendezvous) Wait(ctx context.Context, s *Session) (Peer, error) {
	if s.SID == "" || !time.Now().Before(s.ExpiresAt) {
		return Peer{}, ErrExpired
	}
	ctx, cancel := context.WithDeadline(ctx, s.ExpiresAt.Add(3*time.Second))
	defer cancel()
	var reply struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := c.request(ctx, "POST", "/scan/getPhone", map[string]string{"sid": s.SID}, &reply); err != nil {
		return Peer{}, fmt.Errorf("scan result: %w", err)
	}
	if reply.Code != nil && *reply.Code == 10003 {
		return Peer{}, ErrExpired
	}
	if reply.Code == nil || *reply.Code != 0 {
		return Peer{}, errors.New("scan result rejected by server")
	}
	var nested string
	if json.Unmarshal(reply.Data, &nested) != nil {
		return Peer{}, errors.New("invalid nested scan result")
	}
	var data struct {
		IP           []string `json:"ip"`
		ConnectionID string   `json:"connectionId"`
	}
	if json.Unmarshal([]byte(nested), &data) != nil || len(data.IP) == 0 || len(data.IP) > 16 {
		return Peer{}, errors.New("invalid phone endpoint list")
	}
	peer := Peer{ConnectionID: data.ConnectionID}
	if len(data.ConnectionID) > 256 || strings.ContainsAny(data.ConnectionID, "\r\n\x00") {
		return Peer{}, errors.New("invalid connection identifier")
	}
	for _, value := range data.IP {
		addr, err := netip.ParseAddr(value)
		if err != nil || addr.Zone() != "" {
			peer.IgnoredAddresses++
			continue
		}
		addr = addr.Unmap()
		if !addr.IsPrivate() {
			peer.IgnoredAddresses++
			continue
		}
		peer.Addresses = append(peer.Addresses, addr)
	}
	if len(peer.Addresses) == 0 {
		return Peer{}, fmt.Errorf("scan result contains no private LAN addresses (%d entries ignored)", peer.IgnoredAddresses)
	}
	return peer, nil
}
