package remotepc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const AsiaGateway = "https://asia-remote-desktop-backend.vivoglobal.com"

type Region struct {
	BackendDomain   string `json:"backendDomain"`
	WebsocketDomain string `json:"websocketDomain"`
	MonitorDomain   string `json:"monitorDomain"`
	MachineArea     string `json:"machineArea"`
}

// Registration preserves the shipped request fields. Linux has no established
// deviceType value; callers must explicitly choose an interoperability experiment.
type Registration struct {
	DeviceType        int    `json:"deviceType"`
	Identity          string `json:"identity"`
	DeviceDefaultName string `json:"deviceDefaultName"`
	Version           string `json:"version"`
	DeviceToken       string `json:"deviceToken,omitempty"`
	PCDeviceID        string `json:"pcDeviceId"`
	PriDeviceToken    string `json:"priDeviceToken"`
	CountryCode       string `json:"countryCode"`
}

type Device struct {
	DeviceNo       string `json:"deviceNo"`
	DeviceToken    string `json:"deviceToken"`
	PriDeviceToken string `json:"priDeviceToken"`
	TempPassword   string `json:"tempPassword"`
	DeviceType     int    `json:"deviceType"`
}

// SIPAccount holds plaintext credentials and RTC key material. Callers must keep
// it in memory and exclude it from status responses, logs, and command arguments.
type SIPAccount struct {
	SIPUserName       string `json:"sipUserName"`
	SIPPassword       string `json:"sipPasswd"`
	SIPDomain         string `json:"sipDomain"`
	SIPDomainProxy    string `json:"sipDomainProxy"`
	STUNAddress       string `json:"stunAddr"`
	CipherKey         string `json:"cipherKey"`
	IV                string `json:"iv"`
	AAD               string `json:"aad"`
	Version           int    `json:"version"`
	ConnectedDeviceNo string `json:"connTargetDeviceNo"`
}

type Client struct {
	http      *http.Client
	cipher    *Cipher
	bootstrap string
}

func NewClient(client *http.Client) (*Client, error) {
	cipher, err := NewCipher()
	if err != nil {
		return nil, err
	}
	if client == nil {
		transport := http.DefaultTransport
		if standard, ok := transport.(*http.Transport); ok {
			bounded := standard.Clone()
			bounded.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
			bounded.TLSHandshakeTimeout = 5 * time.Second
			transport = bounded
		}
		client = &http.Client{Transport: transport}
	}
	copy := *client
	if copy.Timeout <= 0 || copy.Timeout > 10*time.Second {
		copy.Timeout = 10 * time.Second
	}
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &copy, cipher: cipher, bootstrap: AsiaGateway}, nil
}

func validBase(value, scheme string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == scheme && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/")
}

func validCountry(country string) bool {
	return len(country) == 2 && country[0] >= 'A' && country[0] <= 'Z' && country[1] >= 'A' && country[1] <= 'Z'
}

func validValue(value string, max int) bool {
	return len(value) <= max && !strings.ContainsAny(value, "\r\n\x00")
}

func (c *Client) request(ctx context.Context, method, base, path string, query url.Values, body any, deviceToken, country string, target any) error {
	if !validBase(base, "https") {
		return errors.New("invalid remote HTTPS backend")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("cannot encode remote request")
		}
	}
	u := strings.TrimSuffix(base, "/") + path
	if len(query) != 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil {
		return errors.New("cannot construct remote request")
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	if deviceToken != "" {
		req.Header.Set("deviceToken", deviceToken)
	}
	if country != "" {
		req.Header.Set("countryCode", country)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("remote backend network/TLS request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("remote backend HTTP status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 512*1024+1))
	if err != nil {
		return errors.New("cannot read remote response")
	}
	if len(raw) > 512*1024 {
		return errors.New("remote response exceeds size limit")
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return errors.New("invalid remote response envelope")
	}
	if *envelope.Code != 0 {
		return fmt.Errorf("remote backend rejected request (code %d)", *envelope.Code)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, target) != nil {
		return errors.New("invalid remote response data")
	}
	return nil
}

func (c *Client) ResolveRegion(ctx context.Context, countryCode string, deviceType int) (Region, error) {
	var region Region
	if !validCountry(countryCode) || (deviceType != 1 && deviceType != 3) {
		return region, errors.New("invalid remote region request")
	}
	query := url.Values{"deviceType": {fmt.Sprint(deviceType)}, "env": {"prd"}, "countryCode": {countryCode}, "cipherText": {c.cipher.Encrypt("ok")}}
	if err := c.request(ctx, "GET", c.bootstrap, "/remote-desk/v1/region/analysis", query, nil, "", "", &region); err != nil {
		return Region{}, err
	}
	if !validBase(region.BackendDomain, "https") || !validBase(region.WebsocketDomain, "wss") || (region.MonitorDomain != "" && !validBase(region.MonitorDomain, "https")) {
		return Region{}, errors.New("invalid remote regional domains")
	}
	return region, nil
}

func (c *Client) Register(ctx context.Context, region Region, registration Registration) (Device, error) {
	var device Device
	identity, err := hex.DecodeString(registration.Identity)
	if err != nil || len(identity) != 32 || !validCountry(registration.CountryCode) || (registration.DeviceType != 1 && registration.DeviceType != 3) || registration.DeviceDefaultName == "" || !validValue(registration.DeviceDefaultName, 256) || registration.Version == "" || !validValue(registration.Version, 64) || registration.PCDeviceID == "" || !validValue(registration.PCDeviceID, 256) || !validValue(registration.DeviceToken, 8192) || !validValue(registration.PriDeviceToken, 8192) {
		return device, errors.New("invalid remote registration")
	}
	body := struct {
		Registration
		CipherText string `json:"cipherText"`
	}{registration, c.cipher.Encrypt("ok")}
	if err := c.request(ctx, "POST", region.BackendDomain, "/remote-desk/v1/device/maodun/regist", nil, body, registration.DeviceToken, registration.CountryCode, &device); err != nil {
		return Device{}, err
	}
	if device.DeviceNo == "" || device.DeviceToken == "" || !validValue(device.DeviceNo, 128) || !validValue(device.DeviceToken, 8192) || !validValue(device.PriDeviceToken, 8192) {
		return Device{}, errors.New("invalid remote device identity")
	}
	if device.TempPassword != "" {
		device.TempPassword, err = c.cipher.Decrypt(device.TempPassword)
		if err != nil {
			return Device{}, err
		}
		if !validValue(device.TempPassword, 256) {
			return Device{}, errors.New("invalid remote temporary password")
		}
	}
	return device, nil
}

func (c *Client) RefreshPassword(ctx context.Context, region Region, device Device, countryCode string) (string, error) {
	if device.DeviceNo == "" || device.DeviceToken == "" || !validValue(device.DeviceNo, 128) || !validValue(device.DeviceToken, 8192) || !validCountry(countryCode) {
		return "", errors.New("invalid remote password refresh")
	}
	query := url.Values{"deviceNo": {device.DeviceNo}, "cipherText": {c.cipher.Encrypt("ok")}}
	var encrypted string
	if err := c.request(ctx, "GET", region.BackendDomain, "/remote-desk/v1/device/maodun/refreshTempPassword", query, nil, device.DeviceToken, countryCode, &encrypted); err != nil {
		return "", err
	}
	password, err := c.cipher.Decrypt(encrypted)
	if err != nil {
		return "", err
	}
	if password == "" || !validValue(password, 256) {
		return "", errors.New("invalid remote temporary password")
	}
	return password, nil
}

func (c *Client) GetSIPAccount(ctx context.Context, region Region, device Device) (SIPAccount, error) {
	var account SIPAccount
	if device.DeviceNo == "" || device.DeviceToken == "" || !validValue(device.DeviceNo, 128) || !validValue(device.DeviceToken, 8192) {
		return account, errors.New("invalid remote SIP identity")
	}
	query := url.Values{"deviceNo": {device.DeviceNo}, "cipherText": {c.cipher.Encrypt("ok")}}
	if err := c.request(ctx, "GET", region.BackendDomain, "/remote-desk/v1/sip/maodun/account", query, nil, device.DeviceToken, "", &account); err != nil {
		return SIPAccount{}, err
	}
	for _, field := range []*string{&account.SIPPassword, &account.CipherKey, &account.IV, &account.AAD} {
		if *field == "" {
			continue
		}
		plain, err := c.cipher.Decrypt(*field)
		if err != nil {
			return SIPAccount{}, errors.New("cannot decode remote SIP credentials")
		}
		*field = plain
	}
	if account.SIPUserName == "" || account.SIPPassword == "" || account.SIPDomain == "" || account.SIPDomainProxy == "" || account.CipherKey == "" || account.IV == "" || account.Version < 0 {
		return SIPAccount{}, errors.New("incomplete remote SIP credentials")
	}
	for _, field := range []string{account.SIPUserName, account.SIPPassword, account.SIPDomain, account.SIPDomainProxy, account.STUNAddress, account.CipherKey, account.IV, account.AAD, account.ConnectedDeviceNo} {
		if !validValue(field, 8192) {
			return SIPAccount{}, errors.New("invalid remote SIP credentials")
		}
	}
	return account, nil
}

func (c *Client) RefreshToken(ctx context.Context, region Region, device Device) (string, error) {
	if device.DeviceNo == "" || device.DeviceToken == "" || !validValue(device.DeviceNo, 128) || !validValue(device.DeviceToken, 8192) {
		return "", errors.New("invalid remote token identity")
	}
	query := url.Values{"deviceNo": {device.DeviceNo}, "deviceToken": {device.DeviceToken}}
	var token string
	if err := c.request(ctx, "GET", region.BackendDomain, "/remote-desk/v1/device/refreshToken", query, nil, device.DeviceToken, "", &token); err != nil {
		return "", err
	}
	if token == "" || !validValue(token, 8192) {
		return "", errors.New("invalid refreshed remote token")
	}
	return token, nil
}
