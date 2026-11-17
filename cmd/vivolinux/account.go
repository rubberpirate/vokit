package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"vivolinux/internal/vivo"
)

const loginCallback = "https://psuite.vivo.com.cn/vbusiness/account/cookie/getHtml"
const accountLoginURL = "https://passport.vivo.com.cn/#/login?client_id=130&redirect_uri=" + loginCallback + "&type=1&theme=light&lang=zh_CN"

var errLoginRequired = errors.New("sign in to the same vivo account as the phone")
var errLoginBrowserCommand = errors.New("login browser context is not ready")

type vivoAccount struct {
	OpenID     string `json:"openId"`
	Token      string `json:"token"`
	DeviceID   string `json:"deviceId"`
	RegionCode string `json:"regionCode"`
}

func validAccount(a vivoAccount) bool {
	for _, v := range []string{a.OpenID, a.Token, a.DeviceID} {
		if v == "" || v == "null" || v == "undefined" || len(v) > 4096 || strings.ContainsAny(v, "\x00\r\n|") {
			return false
		}
	}
	return len(a.RegionCode) <= 16 && !strings.ContainsAny(a.RegionCode, "\x00\r\n")
}

func parseLoginPayload(payload string) (vivoAccount, error) {
	f := strings.Split(payload, "&")
	if len(payload) > 65536 || len(f) < 4 || len(f) > 7 {
		return vivoAccount{}, errLoginRequired
	}
	a := vivoAccount{OpenID: f[0], Token: f[1], DeviceID: f[2], RegionCode: f[3]}
	if !validAccount(a) {
		return vivoAccount{}, errLoginRequired
	}
	return a, nil
}

func isAccountCallback(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host == "psuite.vivo.com.cn" && u.User == nil && u.Path == "/vbusiness/account/cookie/getHtml" && u.Query().Has("openid")
}

func loadAccount(dir string) (vivoAccount, error) {
	var a vivoAccount
	if err := readDirectConfigFile(filepath.Join(dir, "account.json"), uint32(os.Getuid()), true, &a); err != nil || !validAccount(a) {
		return a, errLoginRequired
	}
	return a, nil
}

func savePrivateState(dir, name string, value any) error {
	if _, err := vivo.LoadIdentity(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return errors.New("cannot save private state")
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(value); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		return errors.New("cannot save private state")
	}
	return nil
}

// Sequential CDP commands; neither browser messages nor callback URLs are logged.
type loginCDP struct {
	conn *websocket.Conn
	seq  int
}

func (c *loginCDP) send(ctx context.Context, method string, params any) (int, error) {
	c.seq++
	b, _ := json.Marshal(map[string]any{"id": c.seq, "method": method, "params": params})
	return c.seq, c.conn.Write(ctx, websocket.MessageText, b)
}
func (c *loginCDP) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	id, err := c.send(ctx, method, params)
	if err != nil {
		return nil, errors.New("login browser connection failed")
	}
	for {
		_, b, err := c.conn.Read(ctx)
		if err != nil {
			return nil, errors.New("login browser closed or timed out")
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(b, &reply) != nil {
			return nil, errors.New("invalid login browser response")
		}
		if reply.ID == id {
			if len(reply.Error) > 0 {
				return nil, errLoginBrowserCommand
			}
			return reply.Result, nil
		}
	}
}

func loginAccount(ctx context.Context, dir string) error {
	id, err := vivo.LoadIdentity(dir)
	if err != nil {
		return err
	}
	browser := ""
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "brave-browser"} {
		if p, e := exec.LookPath(name); e == nil {
			browser = p
			break
		}
	}
	if browser == "" {
		return errors.New("install Chromium or Chrome to sign in to vivo")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	profile, err := os.MkdirTemp(dir, "login-browser-")
	if err != nil {
		return errors.New("cannot create private login browser profile")
	}
	defer os.RemoveAll(profile)
	childCtx, stop := context.WithCancel(ctx)
	defer stop()
	child := exec.CommandContext(childCtx, browser, "--user-data-dir="+profile, "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "--no-first-run", "--no-default-browser-check", "about:blank")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
	if err = child.Start(); err != nil {
		return errors.New("cannot start login browser")
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	defer func() {
		select {
		case <-done:
			return
		default:
		}
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	port := 0
	for i := 0; i < 100 && port == 0; i++ {
		if b, e := os.ReadFile(filepath.Join(profile, "DevToolsActivePort")); e == nil {
			port, _ = strconv.Atoi(strings.SplitN(string(b), "\n", 2)[0])
		}
		if port != 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errors.New("login browser closed")
		case <-ticker.C:
		}
	}
	if port < 1 || port > 65535 {
		return errors.New("login browser did not open its private control endpoint")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	base := "127.0.0.1:" + strconv.Itoa(port)
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+base+"/json/list", nil)
	response, err := client.Do(req)
	if err != nil {
		return errors.New("cannot reach login browser")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	response.Body.Close()
	if err != nil {
		return errors.New("cannot read login browser target")
	}
	var pages []struct {
		Type      string `json:"type"`
		WebSocket string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(data, &pages) != nil {
		return errors.New("invalid login browser targets")
	}
	endpoint := ""
	for _, p := range pages {
		if p.Type == "page" {
			u, e := url.Parse(p.WebSocket)
			if e == nil && u.Scheme == "ws" && u.Host == base && u.User == nil {
				endpoint = p.WebSocket
				break
			}
		}
	}
	if endpoint == "" {
		return errors.New("login browser has no page")
	}
	conn, _, err := websocket.Dial(ctx, endpoint, nil)
	if err != nil {
		return errors.New("cannot connect to login browser")
	}
	defer conn.CloseNow()
	conn.SetReadLimit(2 * 1024 * 1024)
	cdp := &loginCDP{conn: conn}
	if _, err = cdp.call(ctx, "Page.enable", map[string]any{}); err != nil {
		return err
	}
	host, _ := os.Hostname()
	info, _ := json.Marshal(map[string]any{"pcDeviceId": id.DeviceID, "pcDeviceName": host, "client_id": 130})
	script := fmt.Sprintf(`addEventListener('DOMContentLoaded',()=>{let e=document.getElementById('originSuite_device_info');if(e)e.setAttribute('value',JSON.stringify(%s));});`, info)
	if _, err = cdp.call(ctx, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": script}); err != nil {
		return err
	}
	if _, err = cdp.send(ctx, "Page.navigate", map[string]any{"url": accountLoginURL}); err != nil {
		return errors.New("cannot open vivo sign-in")
	}
	for {
		select {
		case <-ctx.Done():
			return errors.New("vivo sign-in cancelled or timed out")
		case <-done:
			return errors.New("login browser closed")
		case <-ticker.C:
		}
		result, e := cdp.call(ctx, "Runtime.evaluate", map[string]any{"returnByValue": true, "expression": `JSON.stringify({url:location.href,value:document.querySelector('input[type="hidden"]')?.getAttribute('value')||''})`})
		if e != nil {
			if errors.Is(e, errLoginBrowserCommand) {
				continue
			}
			return e
		}
		var outer struct {
			Result struct {
				Value string `json:"value"`
			} `json:"result"`
		}
		var page struct {
			URL   string `json:"url"`
			Value string `json:"value"`
		}
		if json.Unmarshal(result, &outer) != nil || json.Unmarshal([]byte(outer.Result.Value), &page) != nil || !isAccountCallback(page.URL) {
			continue
		}
		account, e := parseLoginPayload(page.Value)
		if e != nil {
			continue
		}
		return savePrivateState(dir, "account.json", account)
	}
}

func runAccount(ctx context.Context, command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	dir := flags.String("state-dir", "build/pairing", "private pairing state")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("account commands take flags only")
	}
	switch command {
	case "account-status":
		_, err := loadAccount(*dir)
		return printJSON(map[string]bool{"logged_in": err == nil})
	case "account-login":
		if err := loginAccount(ctx, *dir); err != nil {
			return err
		}
		return printJSON(map[string]bool{"logged_in": true})
	case "account-logout":
		if _, err := vivo.LoadIdentity(*dir); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(*dir, "account.json")); err != nil && !os.IsNotExist(err) {
			return errors.New("cannot remove local account")
		}
		if err := forgetRemembered(*dir); err != nil {
			return err
		}
		return printJSON(map[string]bool{"logged_in": false})
	}
	return errors.New("unknown account command")
}
