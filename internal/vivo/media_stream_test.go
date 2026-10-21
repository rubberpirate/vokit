package vivo

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectMediaStreamUsesAuthenticatedRawRoute(t *testing.T) {
	body := strings.Repeat("media", maxResponseBytes/5+1000)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download/down_files" || r.URL.Query().Get("path") != "/storage/emulated/0/a b.mp4" || r.Header.Get("newToken") != "ephemeral" || r.Header.Get("Range") != "bytes=0-" {
			t.Error("wrong direct media contract")
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	client, _ := newPhoneClientTrust(server.URL, "", true)
	defer client.Close()
	response, err := client.OpenMediaStream(context.Background(), &Session{Token: "ephemeral"}, FileEntry{FileName: "a b.mp4", SavePath: "/storage/emulated/0/a b.mp4", FileSize: int64(len(body))}, "bytes=0-")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != body {
		t.Fatal("stream buffered or truncated", err, len(data))
	}
}
