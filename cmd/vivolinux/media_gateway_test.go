package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"vivolinux/internal/vivo"
)

func TestMediaGatewayStreamingRangesAndRevocation(t *testing.T) {
	for _, respectsRange := range []bool{true, false} {
		t.Run(fmt.Sprint(respectsRange), func(t *testing.T) {
			var calls atomic.Int32
			allowed := true
			payload := strings.Repeat("0123456789", 10000)
			gateway, err := newMediaGateway(context.Background(), func(ctx context.Context, entry vivo.FileEntry, value string) (*http.Response, error) {
				calls.Add(1)
				header := make(http.Header)
				body := payload
				status := http.StatusOK
				if respectsRange && value != "" {
					start, end, err := mediaRange(value, int64(len(payload)))
					if err != nil {
						t.Fatal(err)
					}
					body = payload[start : end+1]
					status = http.StatusPartialContent
					header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
			}, func() bool { return allowed })
			if err != nil {
				t.Fatal(err)
			}
			defer gateway.Close()
			entry := vivo.FileEntry{FileName: "fixture.mp4", SavePath: "/storage/emulated/0/fixture.mp4", FileSize: int64(len(payload))}
			ticket, err := gateway.register(entry)
			if err != nil {
				t.Fatal(err)
			}
			url := ticket.(map[string]any)["url"].(string)
			if !strings.HasPrefix(url, "http://127.0.0.1:") || strings.Contains(url, entry.FileName) {
				t.Fatal("unsafe preview URL")
			}
			for _, item := range []struct {
				method, value string
				status        int
				expected      string
			}{
				{"HEAD", "", 200, ""}, {"GET", "bytes=20-29", 206, payload[20:30]},
				{"GET", "bytes=-7", 206, payload[len(payload)-7:]}, {"GET", "bytes=99990-", 206, payload[99990:]},
				{"GET", "bytes=1-2,4-5", 416, ""}, {"GET", "bytes=100000-", 416, ""},
			} {
				req, _ := http.NewRequest(item.method, url, nil)
				req.Header.Set("Range", item.value)
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				data, _ := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != item.status || string(data) != item.expected {
					t.Fatalf("%s %s: status %d body length %d", item.method, item.value, response.StatusCode, len(data))
				}
			}
			if calls.Load() != 3 {
				t.Fatal("HEAD or invalid ranges opened phone data channel")
			}
			allowed = false
			response, _ := http.Get(url)
			response.Body.Close()
			if response.StatusCode != 403 {
				t.Fatal("revoked permission accepted")
			}
			allowed = true
			req, _ := http.NewRequest("DELETE", url, nil)
			response, _ = http.DefaultClient.Do(req)
			response.Body.Close()
			response, _ = http.Get(url)
			response.Body.Close()
			if response.StatusCode != 404 {
				t.Fatal("deleted ticket accepted")
			}
			if _, err = gateway.register(vivo.FileEntry{FileName: "bad.sh", SavePath: "/bad.sh", FileSize: 10}); err == nil {
				t.Fatal("non-media accepted")
			}
		})
	}
}

func TestMediaGatewayDeliversBeforePhoneEOF(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	gateway, _ := newMediaGateway(ctx, func(context.Context, vivo.FileEntry, string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader, ContentLength: 128 << 10}, nil
	}, func() bool { return true })
	defer gateway.Close()
	ticket, _ := gateway.register(vivo.FileEntry{FileName: "live.mp4", SavePath: "/live.mp4", FileSize: 128 << 10})
	sent := make(chan struct{})
	go func() { _, _ = writer.Write([]byte(strings.Repeat("a", 64<<10))); close(sent) }()
	response, err := http.Get(ticket.(map[string]any)["url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, 100)
	if _, err = io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	<-sent
	// The phone has not sent its remaining bytes or EOF, yet the viewer has data.
	response.Body.Close()
	writer.Close()
}

func TestMediaGatewayChecksUpstreamRange(t *testing.T) {
	gateway, _ := newMediaGateway(context.Background(), func(context.Context, vivo.FileEntry, string) (*http.Response, error) {
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {"bytes 0-9/10"}}, Body: io.NopCloser(strings.NewReader("0123456789")), ContentLength: 10}, nil
	}, func() bool { return true })
	defer gateway.Close()
	ticket, _ := gateway.register(vivo.FileEntry{FileName: "test.mp4", SavePath: "/test.mp4", FileSize: 100})
	req := httptest.NewRequest("GET", ticket.(map[string]any)["url"].(string), nil)
	req.Header.Set("Range", "bytes=0-9")
	result := httptest.NewRecorder()
	gateway.ServeHTTP(result, req)
	if result.Code != 502 {
		t.Fatal("mismatching content range accepted")
	}
}
