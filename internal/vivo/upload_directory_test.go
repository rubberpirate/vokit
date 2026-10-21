package vivo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestResolveUploadDirectoryClassifiesPermission(t *testing.T) {
	for _, status := range []int{445, http.StatusForbidden} {
		client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		_, err := client.ResolveUploadDirectory(context.Background(), &Session{Token: "secret"}, "/")
		if !errors.Is(err, ErrFilePermission) {
			t.Fatal("permission rejection was not classified", err)
		}
	}
}

func TestResolveUploadDirectoryUsesBodyResultPath(t *testing.T) {
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/pc_file_manager/get_path" || r.Header.Get("newToken") != "secret" || r.Header.Get("X-ES-HTTP-VERSION") != "1" {
			t.Error("incorrect default upload directory request")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["id"] != "1313" || body["path_type"] != "" {
			t.Error("incorrect default upload directory body")
		}
		fmt.Fprint(w, `{"result_path":"/storage/emulated/0/Download"}`)
	})
	directory, err := client.ResolveUploadDirectory(context.Background(), &Session{Token: "secret"}, "/")
	if err != nil || directory != "/storage/emulated/0/Download" {
		t.Fatal("default upload directory not resolved", err)
	}
}

func TestResolveUploadDirectoryRejectsUnsafeOrWrappedPaths(t *testing.T) {
	for _, body := range []string{
		`{"result_path":"/"}`, `{"result_path":"/storage/../private"}`,
		`{"result_path":"relative"}`, `{"result_path":"/storage//file"}`,
		`{"result_path":"/storage/\u0000"}`, `{"result_path":"/storage\\private"}`,
		`{"code":"0000","data":{"result_path":"/storage/emulated/0"}}`,
		`{}`, `not-json-secret`, strings.Repeat("x", maxResponseBytes+1),
	} {
		client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
		_, err := client.ResolveUploadDirectory(context.Background(), &Session{Token: "secret"}, "/")
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Fatal("unsafe upload directory result", err)
		}
	}
}

func TestResolveUploadDirectoryExplicitPathNeedsNoRequest(t *testing.T) {
	client := fileTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("explicit directory caused phone request") })
	session := &Session{Token: "secret"}
	if got, err := client.ResolveUploadDirectory(context.Background(), session, "/storage/emulated/0/Download"); err != nil || got != "/storage/emulated/0/Download" {
		t.Fatal("explicit upload directory changed", err)
	}
	for _, invalid := range []string{"", "relative", "/storage/../private", "/storage//file"} {
		if _, err := client.ResolveUploadDirectory(context.Background(), session, invalid); err == nil {
			t.Fatal("invalid upload directory accepted")
		}
	}
}

func TestResolveUploadDirectoryPrefersNegotiatedStorageRoot(t *testing.T) {
	client := fileTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("negotiated root caused default lookup") })
	directory, err := client.ResolveUploadDirectory(context.Background(), &Session{Token: "secret", StorageRoot: "/storage/emulated/0"}, "/")
	if err != nil || directory != "/storage/emulated/0" {
		t.Fatal("negotiated root not used", err)
	}
}
