package remotepc

import (
	"bufio"
	"context"
	"crypto/md5" // SIP's captured Digest challenge explicitly requires MD5.
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxSIPHeader = 32 * 1024
const maxSIPBody = 1024 * 1024

type SIPHeader struct{ Name, Value string }
type SIPMessage struct {
	StartLine string
	Headers   []SIPHeader
	Body      []byte
}

func (m SIPMessage) Header(name string) string {
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

func (m SIPMessage) Method() string {
	if strings.HasPrefix(m.StartLine, "SIP/2.0 ") {
		return ""
	}
	method, _, _ := strings.Cut(m.StartLine, " ")
	return method
}

func (m SIPMessage) StatusCode() int {
	fields := strings.Fields(m.StartLine)
	if len(fields) < 2 || fields[0] != "SIP/2.0" {
		return 0
	}
	code, _ := strconv.Atoi(fields[1])
	return code
}

// ReadSIPMessage reads exactly one Content-Length framed message, including
// compact header names and folded header values. TLS stream boundaries are irrelevant.
func ReadSIPMessage(reader *bufio.Reader) (SIPMessage, error) {
	var msg SIPMessage
	total := 0
	readLine := func() (string, error) {
		var line []byte
		for {
			part, err := reader.ReadSlice('\n')
			total += len(part)
			if total > maxSIPHeader {
				return "", errors.New("SIP header exceeds size limit")
			}
			line = append(line, part...)
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil {
				return "", err
			}
			if len(line) < 2 || line[len(line)-2] != '\r' {
				return "", errors.New("invalid SIP line ending")
			}
			return string(line[:len(line)-2]), nil
		}
	}
	for msg.StartLine == "" {
		line, err := readLine()
		if err != nil {
			return msg, err
		}
		msg.StartLine = line
		if line == "" {
			total = 0
		} // CRLF transport keepalive.
	}
	fields := strings.Split(msg.StartLine, " ")
	if len(fields) < 3 || (fields[0] != "SIP/2.0" && (len(fields) != 3 || fields[2] != "SIP/2.0")) {
		return msg, errors.New("invalid SIP start line")
	}
	if fields[0] == "SIP/2.0" {
		code, err := strconv.Atoi(fields[1])
		if err != nil || len(fields[1]) != 3 || code < 100 || code > 699 {
			return msg, errors.New("invalid SIP response status")
		}
	} else if !sipToken(fields[0]) || fields[0] != strings.ToUpper(fields[0]) || (!strings.HasPrefix(fields[1], "sip:") && !strings.HasPrefix(fields[1], "sips:")) || strings.ContainsAny(fields[1], "\t\x00") {
		return msg, errors.New("invalid SIP request start line")
	}
	if strings.ContainsAny(msg.StartLine, "\r\n\x00") {
		return msg, errors.New("invalid SIP start line")
	}
	for {
		line, err := readLine()
		if err != nil {
			return SIPMessage{}, err
		}
		if line == "" {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(msg.Headers) == 0 {
				return SIPMessage{}, errors.New("invalid SIP header continuation")
			}
			msg.Headers[len(msg.Headers)-1].Value += " " + strings.TrimSpace(line)
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !sipToken(name) || strings.ContainsAny(value, "\r\n\x00") {
			return SIPMessage{}, errors.New("invalid SIP header")
		}
		switch strings.ToLower(name) {
		case "l":
			name = "Content-Length"
		case "v":
			name = "Via"
		case "f":
			name = "From"
		case "t":
			name = "To"
		case "i":
			name = "Call-ID"
		case "m":
			name = "Contact"
		case "c":
			name = "Content-Type"
		}
		msg.Headers = append(msg.Headers, SIPHeader{name, strings.TrimSpace(value)})
	}
	length := 0
	found := false
	for _, h := range msg.Headers {
		if strings.EqualFold(h.Name, "Content-Length") {
			if found {
				return SIPMessage{}, errors.New("duplicate SIP Content-Length")
			}
			found = true
			n, err := strconv.Atoi(h.Value)
			if err != nil || n < 0 || n > maxSIPBody {
				return SIPMessage{}, errors.New("invalid SIP Content-Length")
			}
			length = n
		}
	}
	msg.Body = make([]byte, length)
	if _, err := io.ReadFull(reader, msg.Body); err != nil {
		return SIPMessage{}, err
	}
	return msg, nil
}

func sipToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-.!%*_+`'~", r)) {
			return false
		}
	}
	return true
}

func encodeSIP(msg SIPMessage) ([]byte, error) {
	if len(msg.Body) > maxSIPBody || msg.StartLine == "" || len(msg.StartLine) > maxSIPHeader-64 || strings.ContainsAny(msg.StartLine, "\r\n\x00") {
		return nil, errors.New("invalid outgoing SIP message")
	}
	var b strings.Builder
	b.WriteString(msg.StartLine + "\r\n")
	for _, h := range msg.Headers {
		if !sipToken(h.Name) || strings.ContainsAny(h.Value, "\r\n\x00") {
			return nil, errors.New("invalid outgoing SIP header")
		}
		if strings.EqualFold(h.Name, "Content-Length") || strings.EqualFold(h.Name, "l") {
			continue
		}
		b.WriteString(h.Name + ": " + h.Value + "\r\n")
		if b.Len() > maxSIPHeader-64 {
			return nil, errors.New("outgoing SIP header exceeds size limit")
		}
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(msg.Body))
	return append([]byte(b.String()), msg.Body...), nil
}

type SIPConfig struct {
	// Endpoint defaults to SIPDomainProxy. ServerName defaults to its hostname,
	// never the logical SIP account domain. Certificate verification is mandatory.
	Endpoint   string
	ServerName string
	TLSConfig  *tls.Config
	// Requests run serially on a bounded callback queue, independently of reads.
	OnRequest func(*SIPClient, SIPMessage)
}

type SIPClient struct {
	conn                         net.Conn
	account                      SIPAccount
	endpoint, local, callID, tag string
	ctx                          context.Context
	cancel                       context.CancelFunc
	done                         chan struct{}
	onRequest                    func(*SIPClient, SIPMessage)
	requests                     chan SIPMessage
	mu                           sync.Mutex
	pending                      map[string]chan SIPMessage
	cseq                         uint32
	err                          error
	writeMu                      sync.Mutex
	registerMu                   sync.Mutex
	challenge                    digestChallenge
	nonceCount                   uint32
	cnonce                       string
	renewOnce                    sync.Once
	renewEvery                   time.Duration
}

func sipRandom() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("SIP randomness failed")
	}
	return hex.EncodeToString(b[:]), nil
}

func DialSIP(ctx context.Context, account SIPAccount, config SIPConfig) (*SIPClient, error) {
	if account.SIPUserName == "" || !sipToken(account.SIPUserName) || account.SIPPassword == "" || !validValue(account.SIPPassword, 8192) || account.SIPDomain == "" || strings.ContainsAny(account.SIPDomain, " <>;\r\n\x00") {
		return nil, errors.New("invalid SIP account")
	}
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = account.SIPDomainProxy
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("invalid SIP TLS endpoint")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if config.TLSConfig != nil {
		tlsConfig = config.TLSConfig.Clone()
	}
	if tlsConfig.InsecureSkipVerify {
		return nil, errors.New("SIP TLS certificate verification is required")
	}
	if tlsConfig.MinVersion < tls.VersionTLS12 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}
	tlsConfig.ServerName = config.ServerName
	if tlsConfig.ServerName == "" {
		tlsConfig.ServerName = host
	}
	callID, err := sipRandom()
	if err != nil {
		return nil, err
	}
	tag, err := sipRandom()
	if err != nil {
		return nil, err
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 10 * time.Second}, Config: tlsConfig}
	conn, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("SIP TLS connection failed")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c := &SIPClient{conn: conn, account: account, endpoint: endpoint, local: conn.LocalAddr().String(), callID: callID, tag: tag, ctx: lifetime, cancel: cancel, done: make(chan struct{}), onRequest: config.OnRequest, requests: make(chan SIPMessage, 16), pending: make(map[string]chan SIPMessage), renewEvery: 100 * time.Second}
	go c.readLoop()
	go c.callbackLoop()
	return c, nil
}

func (c *SIPClient) Close() error          { c.stop(nil); return nil }
func (c *SIPClient) Done() <-chan struct{} { return c.done }
func (c *SIPClient) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
func (c *SIPClient) stop(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return
	default:
	}
	c.err = err
	c.cancel()
	c.conn.Close()
	close(c.done)
}

func transactionKey(msg SIPMessage) string {
	return msg.Header("Call-ID") + "\x00" + msg.Header("CSeq")
}
func (c *SIPClient) readLoop() {
	reader := bufio.NewReader(c.conn)
	for {
		msg, err := ReadSIPMessage(reader)
		if err != nil {
			c.stop(errors.New("SIP connection ended or received malformed message"))
			return
		}
		if msg.StatusCode() != 0 {
			if msg.StatusCode() < 200 {
				continue
			}
			c.mu.Lock()
			ch := c.pending[transactionKey(msg)]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- msg:
				default:
				}
			}
			continue
		}
		select {
		case c.requests <- msg:
		case <-c.ctx.Done():
			return
		default:
			c.stop(errors.New("SIP request callback queue exceeded"))
			return
		}
	}
}

func (c *SIPClient) callbackLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.requests:
			if c.onRequest != nil {
				c.onRequest(c, msg)
			}
		}
	}
}

func (c *SIPClient) write(ctx context.Context, msg SIPMessage) error {
	data, err := encodeSIP(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.conn.SetWriteDeadline(deadline)
	for len(data) > 0 {
		n, err := c.conn.Write(data)
		if err != nil {
			c.stop(errors.New("SIP write failed"))
			return errors.New("SIP write failed")
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// SendRequest adds transaction defaults; supplied dialog headers (From, To,
// Call-ID, CSeq, Route) are retained. ACK is sent without awaiting a response.
func (c *SIPClient) SendRequest(ctx context.Context, method, uri string, headers []SIPHeader, body []byte) (SIPMessage, error) {
	if !sipToken(method) || method != strings.ToUpper(method) || (!strings.HasPrefix(uri, "sip:") && !strings.HasPrefix(uri, "sips:")) || strings.ContainsAny(uri, " \t\r\n\x00") || len(uri) > 2048 {
		return SIPMessage{}, errors.New("invalid SIP request target")
	}
	branch, err := sipRandom()
	if err != nil {
		return SIPMessage{}, err
	}
	c.mu.Lock()
	c.cseq++
	seq := c.cseq
	c.mu.Unlock()
	msg := SIPMessage{StartLine: method + " " + uri + " SIP/2.0", Headers: append([]SIPHeader{}, headers...), Body: body}
	defaults := []SIPHeader{{"Via", "SIP/2.0/TLS " + c.local + ";rport;branch=z9hG4bK" + branch}, {"From", "<sip:" + c.account.SIPUserName + "@" + c.account.SIPDomain + ">;tag=" + c.tag}, {"To", "<sip:" + c.account.SIPUserName + "@" + c.account.SIPDomain + ">"}, {"Call-ID", c.callID}, {"CSeq", fmt.Sprintf("%d %s", seq, method)}, {"Max-Forwards", "70"}, {"User-Agent", "vivo-pc-3.0.4"}}
	for _, h := range defaults {
		if msg.Header(h.Name) == "" {
			msg.Headers = append(msg.Headers, h)
		}
	}
	if method == "ACK" {
		return SIPMessage{}, c.write(ctx, msg)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	key := transactionKey(msg)
	ch := make(chan SIPMessage, 1)
	c.mu.Lock()
	if _, exists := c.pending[key]; exists {
		c.mu.Unlock()
		return SIPMessage{}, errors.New("duplicate SIP transaction")
	}
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.write(ctx, msg); err != nil {
		return SIPMessage{}, err
	}
	select {
	case response := <-ch:
		return response, nil
	case <-ctx.Done():
		return SIPMessage{}, ctx.Err()
	case <-c.ctx.Done():
		return SIPMessage{}, errors.New("SIP connection closed")
	}
}

func (c *SIPClient) SendResponse(ctx context.Context, request SIPMessage, status int, reason string, headers []SIPHeader, body []byte) error {
	if request.Method() == "" || status < 100 || status > 699 || reason == "" || strings.ContainsAny(reason, "\r\n\x00") {
		return errors.New("invalid SIP response")
	}
	msg := SIPMessage{StartLine: fmt.Sprintf("SIP/2.0 %d %s", status, reason), Headers: append([]SIPHeader{}, headers...), Body: body}
	for _, name := range []string{"Via", "From", "To", "Call-ID", "CSeq"} {
		if msg.Header(name) != "" {
			continue
		}
		found := false
		for _, h := range request.Headers {
			if strings.EqualFold(h.Name, name) {
				if name == "To" && status > 100 && !strings.Contains(strings.ToLower(h.Value), ";tag=") {
					h.Value += ";tag=" + c.tag
				}
				msg.Headers = append(msg.Headers, h)
				found = true
			}
		}
		if !found {
			return errors.New("SIP request lacks response routing headers")
		}
	}
	if request.Method() == "INVITE" && status >= 200 && status < 300 && msg.Header("Contact") == "" {
		msg.Headers = append(msg.Headers, SIPHeader{"Contact", "<sip:" + c.account.SIPUserName + "@" + c.local + ";transport=TLS>"})
	}
	return c.write(ctx, msg)
}

func (c *SIPClient) Register(ctx context.Context) error {
	if err := c.register(ctx); err != nil {
		return err
	}
	c.renewOnce.Do(func() {
		go func() {
			timer := time.NewTicker(c.renewEvery)
			defer timer.Stop()
			for {
				select {
				case <-c.ctx.Done():
					return
				case <-timer.C:
					if err := c.register(c.ctx); err != nil {
						c.stop(errors.New("SIP registration renewal failed"))
						return
					}
				}
			}
		}()
	})
	return nil
}

func (c *SIPClient) register(ctx context.Context) error {
	c.registerMu.Lock()
	defer c.registerMu.Unlock()
	uri := "sip:" + c.endpoint
	for attempt := 0; attempt < 3; attempt++ {
		headers := []SIPHeader{{"Contact", "<sip:" + c.account.SIPUserName + "@" + c.local + ";transport=TLS>"}, {"Expires", "140"}, {"X-VIVO-PC-Control", "true"}, {"Supported", "replaces,outbound,gruu"}}
		if c.challenge.nonce != "" {
			c.nonceCount++
			headers = append(headers, SIPHeader{"Authorization", c.challenge.authorization(c.account.SIPUserName, c.account.SIPPassword, "REGISTER", uri, c.cnonce, c.nonceCount)})
		}
		response, err := c.SendRequest(ctx, "REGISTER", uri, headers, nil)
		if err != nil {
			return err
		}
		if response.StatusCode() >= 200 && response.StatusCode() < 300 {
			return nil
		}
		if response.StatusCode() != 401 {
			return fmt.Errorf("SIP registration rejected (status %d)", response.StatusCode())
		}
		challenge, err := parseDigestChallenge(response.Header("WWW-Authenticate"))
		if err != nil {
			return err
		}
		c.challenge = challenge
		c.nonceCount = 0
		c.cnonce, err = sipRandom()
		if err != nil {
			return err
		}
	}
	return errors.New("SIP registration authentication failed")
}

type digestChallenge struct{ realm, nonce, opaque string }

func parseDigestChallenge(value string) (digestChallenge, error) {
	var result digestChallenge
	scheme, rest, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "Digest") {
		return result, errors.New("unsupported SIP authentication challenge")
	}
	params := map[string]string{}
	for strings.TrimSpace(rest) != "" {
		rest = strings.TrimLeft(rest, " \t,")
		name, tail, ok := strings.Cut(rest, "=")
		if !ok {
			return result, errors.New("invalid SIP digest challenge")
		}
		name = strings.ToLower(strings.TrimSpace(name))
		if !sipToken(name) {
			return result, errors.New("invalid SIP digest parameter")
		}
		tail = strings.TrimSpace(tail)
		var val string
		if strings.HasPrefix(tail, "\"") {
			var b strings.Builder
			escaped := false
			end := -1
			for i := 1; i < len(tail); i++ {
				if escaped {
					b.WriteByte(tail[i])
					escaped = false
					continue
				}
				if tail[i] == '\\' {
					escaped = true
					continue
				}
				if tail[i] == '"' {
					end = i
					break
				}
				b.WriteByte(tail[i])
			}
			if end < 0 {
				return result, errors.New("invalid SIP quoted digest parameter")
			}
			val = b.String()
			rest = strings.TrimSpace(tail[end+1:])
			if rest != "" && !strings.HasPrefix(rest, ",") {
				return result, errors.New("invalid SIP digest separator")
			}
		} else {
			val, rest, _ = strings.Cut(tail, ",")
			val = strings.TrimSpace(val)
		}
		if _, exists := params[name]; exists {
			return result, errors.New("duplicate SIP digest parameter")
		}
		params[name] = val
	}
	if params["realm"] == "" || params["nonce"] == "" || (params["algorithm"] != "" && !strings.EqualFold(params["algorithm"], "MD5")) {
		return result, errors.New("unsupported SIP digest parameters")
	}
	auth := false
	for _, qop := range strings.Split(params["qop"], ",") {
		if strings.TrimSpace(qop) == "auth" {
			auth = true
		}
	}
	if !auth {
		return result, errors.New("SIP digest qop auth required")
	}
	for _, v := range params {
		if !validValue(v, 8192) {
			return result, errors.New("invalid SIP digest parameter")
		}
	}
	return digestChallenge{params["realm"], params["nonce"], params["opaque"]}, nil
}

func digestMD5(value string) string { sum := md5.Sum([]byte(value)); return hex.EncodeToString(sum[:]) }
func digestQuote(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value) + "\""
}
func (d digestChallenge) authorization(user, password, method, uri, cnonce string, count uint32) string {
	nc := fmt.Sprintf("%08x", count)
	response := digestMD5(digestMD5(user+":"+d.realm+":"+password) + ":" + d.nonce + ":" + nc + ":" + cnonce + ":auth:" + digestMD5(method+":"+uri))
	value := "Digest username=" + digestQuote(user) + ", realm=" + digestQuote(d.realm) + ", nonce=" + digestQuote(d.nonce) + ", uri=" + digestQuote(uri) + ", response=" + digestQuote(response) + ", algorithm=MD5, cnonce=" + digestQuote(cnonce) + ", qop=auth, nc=" + nc
	if d.opaque != "" {
		value += ", opaque=" + digestQuote(d.opaque)
	}
	return value
}
