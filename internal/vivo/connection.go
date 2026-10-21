package vivo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// phoneConnection keeps negotiation and the WSS upgrade on one TLS connection.
// The phone generates a fresh certificate/key on each TCP connection. A single
// connection lets explicit first-use trust pin the actual token-bearing channel.
// There is deliberately no automatic reconnect or certificate refresh.
type phoneConnection struct {
	mu                        sync.Mutex
	endpoint                  string
	config                    *tls.Config
	conn                      net.Conn
	reader                    *bufio.Reader
	incoming                  *io.LimitedReader
	started, upgraded, closed bool
}

func (p *phoneConnection) CloseIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.conn != nil {
		p.conn.Close()
	}
}

func (p *phoneConnection) RoundTrip(req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if req.Body != nil {
		defer req.Body.Close()
	}
	if req.URL.Scheme != "https" || req.URL.Host != p.endpoint || req.URL.User != nil {
		return nil, errors.New("phone request changed endpoint")
	}
	if p.closed || p.upgraded {
		return nil, errors.New("pinned phone connection is closed or upgraded; rescan required")
	}
	fail := func() {
		p.closed = true
		if p.conn != nil {
			p.conn.Close()
		}
	}
	if !p.started {
		p.started = true
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: p.config}
		conn, err := dialer.DialContext(req.Context(), "tcp", p.endpoint)
		if err != nil {
			fail()
			return nil, err
		}
		p.conn = conn
		p.incoming = &io.LimitedReader{R: conn}
		p.reader = bufio.NewReaderSize(p.incoming, 4096)
	}
	if err := req.Context().Err(); err != nil {
		fail()
		return nil, err
	}
	// Bound the entire HTTP reply, including headers and chunk framing.
	streaming := req.Method == http.MethodGet && req.URL.Path == "/pc_file_manager/download"
	p.incoming.N = maxResponseBytes + 64*1024
	if streaming {
		p.incoming.N = MaxDownloadBytes + 2*1024*1024
	}
	deadline, ok := req.Context().Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if err := p.conn.SetDeadline(deadline); err != nil {
		fail()
		return nil, err
	}
	// Cancellation closes a blocked read/write, even without a context deadline.
	stop := context.AfterFunc(req.Context(), func() { p.conn.Close() })
	stopOnReturn := true
	defer func() {
		if stopOnReturn {
			stop()
		}
	}()
	var response *http.Response
	var err error
	interruptedUpload := false
	if req.Method == http.MethodPost && req.URL.Path == "/upload/drop_file_to_phone" && req.Body != nil {
		// A rejection can arrive while the peer has stopped reading the TAR body.
		written := make(chan error, 1)
		go func() { written <- req.Write(p.conn) }()
		response, err = http.ReadResponse(p.reader, req)
		var writeErr error
		select {
		case writeErr = <-written:
		default:
			req.Body.Close()
			_ = p.conn.SetWriteDeadline(time.Now())
			writeErr = <-written
			interruptedUpload = true
		}
		interruptedUpload = interruptedUpload || writeErr != nil
		if err == nil && writeErr != nil && response.StatusCode == http.StatusOK {
			fail()
			response.Body.Close()
			return nil, writeErr
		}
	} else {
		if err := req.Write(p.conn); err != nil {
			fail()
			return nil, err
		}
		response, err = http.ReadResponse(p.reader, req)
	}
	if err != nil {
		fail()
		return nil, err
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		validUpgrade := req.URL.Path == "/ws/heart-beat" || req.URL.Path == "/mirror/screen" || req.URL.Path == "/mirror/control" || req.URL.Path == "/camera_share/screen" || req.URL.Path == "/camera_share/config"
		if !validUpgrade || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") {
			fail()
			return nil, errors.New("unexpected phone protocol upgrade")
		}
		if !stop() || req.Context().Err() != nil {
			fail()
			return nil, req.Context().Err()
		}
		if err := p.conn.SetDeadline(time.Time{}); err != nil {
			fail()
			return nil, err
		}
		p.upgraded = true
		p.incoming.N = math.MaxInt64 // WSS enforces its own frame/message read limit.
		response.Body = &phoneDuplex{Conn: p.conn, reader: p.reader}
		return response, nil
	}
	if streaming && response.StatusCode == http.StatusOK {
		p.upgraded = true // Body exclusively owns the data connection until Close.
		stopOnReturn = false
		response.Body = &phoneStream{ReadCloser: response.Body, conn: p.conn, stop: stop}
		return response, nil
	}
	// Buffer only a bounded reply. Do not let Body.Close drain arbitrary data.
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		fail()
		response.Body.Close()
		return nil, errors.New("phone response read failed or exceeded size limit")
	}
	response.Body.Close()
	if response.Close || interruptedUpload {
		fail()
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

type phoneDuplex struct {
	net.Conn
	reader *bufio.Reader
}

func (p *phoneDuplex) Read(b []byte) (int, error) { return p.reader.Read(b) }

type phoneStream struct {
	io.ReadCloser
	conn net.Conn
	stop func() bool
	once sync.Once
}

func (s *phoneStream) Close() error {
	var err error
	s.once.Do(func() { s.stop(); s.conn.Close(); err = s.ReadCloser.Close() })
	return err
}
