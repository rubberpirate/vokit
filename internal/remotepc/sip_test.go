package remotepc

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSIPDigestRFC2617(t *testing.T) {
	d, err := parseDigestChallenge(`Digest realm="testrealm@host.com", qop="auth,auth-int", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093", opaque="5ccc069c403ebaf9f0171e9517f40e41"`)
	if err != nil {
		t.Fatal(err)
	}
	auth := d.authorization("Mufasa", "Circle Of Life", "GET", "/dir/index.html", "0a4f113b", 1)
	if !strings.Contains(auth, `response="6629fae49393a05397450978507c4ef1"`) || !strings.Contains(auth, "nc=00000001") || !strings.Contains(auth, `opaque="5ccc069c403ebaf9f0171e9517f40e41"`) {
		t.Fatal("digest differs from RFC2617 fixture")
	}
	for _, bad := range []string{`Basic realm="x"`, `Digest realm="x",nonce="y",qop="auth-int"`, `Digest realm="x",nonce="y",qop="auth",algorithm=SHA-256`, `Digest realm="x",nonce="y",nonce="z",qop="auth"`, `Digest realm="x",nonce="unterminated`, `Digest realm="x",nonce="y" qop=auth`} {
		if _, err := parseDigestChallenge(bad); err == nil {
			t.Fatal("accepted unsupported or malformed digest")
		}
	}
}

func TestSIPStreamFraming(t *testing.T) {
	first := "MESSAGE sip:test@example.test SIP/2.0\r\nv: SIP/2.0/TLS first;branch=a\r\nVia: SIP/2.0/TLS second;branch=b\r\nf: <sip:sender@example.test>;tag=from\r\nt: <sip:test@example.test>\r\ni: call\r\nCSeq: 1 MESSAGE\r\nSubject: folded\r\n value\r\nl: 5\r\n\r\nhello"
	second := "SIP/2.0 200 OK\r\nContent-Length: 0\r\n\r\n"
	r := bufio.NewReaderSize(strings.NewReader("\r\n"+first+second), 16)
	msg, err := ReadSIPMessage(r)
	if err != nil || msg.Method() != "MESSAGE" || string(msg.Body) != "hello" || msg.Header("Subject") != "folded value" || msg.Header("Call-ID") != "call" {
		t.Fatalf("framed request failed: %v", err)
	}
	msg, err = ReadSIPMessage(r)
	if err != nil || msg.StatusCode() != 200 || len(msg.Body) != 0 {
		t.Fatalf("pipelined response failed: %v", err)
	}
	for name, bad := range map[string]string{
		"oversized header": "MESSAGE sip:test@example.test SIP/2.0\r\nX: " + strings.Repeat("x", maxSIPHeader) + "\r\n\r\n",
		"duplicate length": "MESSAGE sip:test@example.test SIP/2.0\r\nContent-Length: 0\r\nl: 1\r\n\r\n",
		"body limit":       fmt.Sprintf("MESSAGE sip:test@example.test SIP/2.0\r\nContent-Length: %d\r\n\r\n", maxSIPBody+1),
		"truncated body":   "MESSAGE sip:test@example.test SIP/2.0\r\nContent-Length: 3\r\n\r\na",
		"line ending":      "MESSAGE sip:test@example.test SIP/2.0\n\n",
		"orphan fold":      "MESSAGE sip:test@example.test SIP/2.0\r\n folded\r\n\r\n",
		"header injection": "MESSAGE sip:test@example.test SIP/2.0\r\nBad Header: yes\r\n\r\n",
		"invalid status":   "SIP/2.0 abc Bad\r\nContent-Length: 0\r\n\r\n",
		"invalid method":   "BAD/METHOD sip:test@example.test SIP/2.0\r\nContent-Length: 0\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadSIPMessage(bufio.NewReader(strings.NewReader(bad))); err == nil {
				t.Fatal("accepted malformed frame")
			}
		})
	}
}

func mockSIPTLS(t *testing.T, session func(net.Conn) error) (string, *tls.Config, <-chan error) {
	t.Helper()
	certServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cert := certServer.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(certServer.Certificate())
	certServer.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		result <- session(conn)
	}()
	return listener.Addr().String(), &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, result
}

func mockWriteSIP(conn net.Conn, msg SIPMessage) error {
	b, err := encodeSIP(msg)
	if err != nil {
		return err
	}
	// Deliberately split the first line across TLS writes.
	for _, part := range [][]byte{b[:7], b[7:]} {
		if _, err := conn.Write(part); err != nil {
			return err
		}
	}
	return nil
}

func mockSIPResponse(request SIPMessage, status int, reason string, extra ...SIPHeader) SIPMessage {
	response := SIPMessage{StartLine: fmt.Sprintf("SIP/2.0 %d %s", status, reason), Headers: extra}
	for _, h := range request.Headers {
		switch strings.ToLower(h.Name) {
		case "via", "from", "to", "call-id", "cseq":
			response.Headers = append(response.Headers, h)
		}
	}
	return response
}

func mockRegister(conn net.Conn, r *bufio.Reader) error {
	first, err := ReadSIPMessage(r)
	if err != nil {
		return err
	}
	if first.Method() != "REGISTER" || first.Header("Authorization") != "" || first.Header("Expires") != "140" || first.Header("X-VIVO-PC-Control") != "true" || !strings.HasPrefix(first.Header("Via"), "SIP/2.0/TLS ") || !strings.Contains(first.Header("Contact"), ";transport=TLS>") {
		return errors.New("unexpected initial REGISTER contract")
	}
	if err := mockWriteSIP(conn, mockSIPResponse(first, 401, "Unauthorized", SIPHeader{"WWW-Authenticate", `Digest realm="synthetic-realm",nonce="synthetic-nonce",algorithm=MD5,qop="auth"`})); err != nil {
		return err
	}
	second, err := ReadSIPMessage(r)
	if err != nil {
		return err
	}
	auth := second.Header("Authorization")
	match := regexp.MustCompile(`cnonce="([^"]+)"`).FindStringSubmatch(auth)
	if len(match) != 2 {
		return errors.New("missing digest cnonce")
	}
	uri := strings.Split(second.StartLine, " ")[1]
	expected := digestChallenge{realm: "synthetic-realm", nonce: "synthetic-nonce"}.authorization("synthetic-user", "synthetic-password", "REGISTER", uri, match[1], 1)
	if auth != expected || second.Header("CSeq") != "2 REGISTER" || first.Header("Call-ID") != second.Header("Call-ID") || first.Header("From") != second.Header("From") || first.Header("Via") == second.Header("Via") {
		return errors.New("incorrect authenticated REGISTER transaction")
	}
	return mockWriteSIP(conn, mockSIPResponse(second, 200, "OK"))
}

func TestSIPTLSRegisterAndDialogMessages(t *testing.T) {
	events := make(chan SIPMessage, 3)
	endpoint, tlsConfig, result := mockSIPTLS(t, func(conn net.Conn) error {
		r := bufio.NewReader(conn)
		if err := mockRegister(conn, r); err != nil {
			return err
		}
		for index, method := range []string{"INVITE", "MESSAGE", "BYE"} {
			request := SIPMessage{StartLine: method + " sip:synthetic-user@logical-domain.invalid SIP/2.0", Headers: []SIPHeader{{"Via", "SIP/2.0/TLS peer.example.test;branch=z9hG4bKsynthetic"}, {"Via", "SIP/2.0/TLS previous.example.test;branch=z9hG4bKprevious"}, {"From", "<sip:phone@logical-domain.invalid>;tag=remote"}, {"To", "<sip:synthetic-user@logical-domain.invalid>"}, {"Call-ID", "synthetic-dialog"}, {"CSeq", fmt.Sprintf("%d %s", index+1, method)}}, Body: []byte("synthetic-body")}
			if err := mockWriteSIP(conn, request); err != nil {
				return err
			}
			response, err := ReadSIPMessage(r)
			if err != nil {
				return err
			}
			if response.StatusCode() != 200 || response.Header("Call-ID") != request.Header("Call-ID") || response.Header("CSeq") != request.Header("CSeq") || response.Header("From") != request.Header("From") || !strings.HasPrefix(response.Header("To"), request.Header("To")+";tag=") {
				return errors.New("incorrect incoming-request response routing")
			}
			vias := 0
			for _, h := range response.Headers {
				if strings.EqualFold(h.Name, "Via") {
					vias++
				}
			}
			if vias != 2 {
				return errors.New("lost response Via chain")
			}
			if index == 0 {
				info, err := ReadSIPMessage(r)
				if err != nil {
					return err
				}
				if info.Method() != "INFO" || info.Header("Call-ID") != "synthetic-dialog" || info.Header("CSeq") != "2 INFO" || info.Header("Route") != "<sip:route.example.test;lr>" || string(info.Body) != "synthetic-info" {
					return errors.New("lost dialog INFO headers")
				}
				if err := mockWriteSIP(conn, mockSIPResponse(info, 200, "OK")); err != nil {
					return err
				}
			}
		}
		return nil
	})
	c, err := DialSIP(context.Background(), SIPAccount{SIPUserName: "synthetic-user", SIPPassword: "synthetic-password", SIPDomain: "logical-domain.invalid", SIPDomainProxy: endpoint}, SIPConfig{TLSConfig: tlsConfig, OnRequest: func(c *SIPClient, msg SIPMessage) {
		if err := c.SendResponse(context.Background(), msg, 200, "OK", nil, nil); err != nil {
			t.Error(err)
		}
		events <- msg
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := c.Register(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-events:
		if msg.Method() != "INVITE" {
			t.Fatal("missing INVITE")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	response, err := c.SendRequest(ctx, "INFO", "sip:phone@logical-domain.invalid", []SIPHeader{{"From", "<sip:synthetic-user@logical-domain.invalid>;tag=local"}, {"To", "<sip:phone@logical-domain.invalid>;tag=remote"}, {"Call-ID", "synthetic-dialog"}, {"CSeq", "2 INFO"}, {"Route", "<sip:route.example.test;lr>"}}, []byte("synthetic-info"))
	if err != nil || response.StatusCode() != 200 {
		t.Fatalf("INFO failed: %v", err)
	}
	for _, method := range []string{"MESSAGE", "BYE"} {
		select {
		case msg := <-events:
			if msg.Method() != method {
				t.Fatalf("expected %s", method)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestSIPRegistrationRenews(t *testing.T) {
	endpoint, tlsConfig, result := mockSIPTLS(t, func(conn net.Conn) error {
		r := bufio.NewReader(conn)
		if err := mockRegister(conn, r); err != nil {
			return err
		}
		renewal, err := ReadSIPMessage(r)
		if err != nil {
			return err
		}
		if renewal.Method() != "REGISTER" || renewal.Header("Expires") != "140" || !strings.Contains(renewal.Header("Authorization"), "nc=00000002") {
			return errors.New("registration was not renewed with incremented nonce count")
		}
		return mockWriteSIP(conn, mockSIPResponse(renewal, 200, "OK"))
	})
	c, err := DialSIP(context.Background(), SIPAccount{SIPUserName: "synthetic-user", SIPPassword: "synthetic-password", SIPDomain: "logical-domain.invalid", SIPDomainProxy: endpoint}, SIPConfig{TLSConfig: tlsConfig})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.renewEvery = 10 * time.Millisecond
	if err := c.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestSIPRejectsUnverifiedTLSAndOutgoingInjection(t *testing.T) {
	account := SIPAccount{SIPUserName: "synthetic-user", SIPPassword: "synthetic-password", SIPDomain: "logical-domain.invalid", SIPDomainProxy: "127.0.0.1:5061"}
	if _, err := DialSIP(context.Background(), account, SIPConfig{TLSConfig: &tls.Config{InsecureSkipVerify: true}}); err == nil {
		t.Fatal("allowed disabled TLS verification")
	}
	if _, err := encodeSIP(SIPMessage{StartLine: "SIP/2.0 200 OK", Headers: []SIPHeader{{"X", "value\r\nInjected: yes"}}}); err == nil {
		t.Fatal("allowed header injection")
	}
	if _, err := ReadSIPMessage(bufio.NewReader(strings.NewReader(""))); !errors.Is(err, io.EOF) {
		t.Fatal("EOF not propagated")
	}
}
