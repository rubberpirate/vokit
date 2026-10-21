package vivo

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testThumbnailPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestFetchThumbnailAuthenticatedEscapedPath(t *testing.T) {
	data := testThumbnailPNG(t, 16, 12)
	entry := FileEntry{FileName: "sample #&?✓.png", SavePath: "/storage/emulated/0/sample #&?✓.png", FileSize: 100}
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/pc_file_manager/thumb" || r.URL.Query().Get("fileUri") != entry.SavePath || r.URL.Query().Get("width") != "160" || r.URL.Query().Get("height") != "120" || len(r.URL.Query()) != 3 || strings.Contains(r.URL.RawQuery, "secret") {
			t.Error("thumbnail query changed path or exposed token")
		}
		if r.Header.Get("newToken") != "secret" || r.Header.Get("X-ES-HTTP-VERSION") != "1" {
			t.Error("missing thumbnail authentication")
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(data)
	})
	thumbnail, err := client.FetchThumbnail(context.Background(), &Session{Token: "secret"}, entry, 160, 120)
	if err != nil || thumbnail.MIME != "image/png" || thumbnail.Width != 16 || thumbnail.Height != 12 || !bytes.Equal(thumbnail.Data, data) {
		t.Fatal("thumbnail result invalid", err)
	}
}

func TestThumbnailRejectsBoundsTypesAndRedirects(t *testing.T) {
	for _, test := range []struct {
		name, mime string
		body       []byte
		status     int
	}{
		{"HTML", "text/html", []byte("<html>secret</html>"), 200},
		{"SVG", "image/svg+xml", []byte("<svg/>"), 200},
		{"MIME mismatch", "text/html", testThumbnailPNG(t, 2, 2), 200},
		{"oversized", "image/png", bytes.Repeat([]byte("x"), maxResponseBytes+1), 200},
		{"huge dimensions", "image/png", testThumbnailPNG(t, 5000, 1), 200},
		{"redirect", "", nil, 302}, {"permission", "", nil, 445},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/pc_file_manager/thumb" {
					t.Error("followed thumbnail redirect")
				}
				w.Header().Set("Content-Type", test.mime)
				w.Header().Set("Location", "/stolen-token")
				w.WriteHeader(test.status)
				w.Write(test.body)
			})
			_, err := client.FetchThumbnail(context.Background(), &Session{Token: "secret"}, FileEntry{FileName: "sample.png", SavePath: "/sample.png", FileSize: 10}, 160, 160)
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "sample") {
				t.Fatal("unsafe thumbnail validation", err)
			}
			if test.status == 445 && !errors.Is(err, ErrFilePermission) {
				t.Fatal("permission error lost", err)
			}
		})
	}
}

func TestThumbnailCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	})
	_, err := client.FetchThumbnail(ctx, &Session{Token: "secret"}, FileEntry{FileName: "sample.png", SavePath: "/sample.png", FileSize: 10}, 160, 160)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("thumbnail cancellation lost", err)
	}
}

func TestPrepareMediaPreviewSafeImageAndVideo(t *testing.T) {
	for _, test := range []struct {
		name, mime string
		data       []byte
	}{
		{"sample.png", "image/png", testThumbnailPNG(t, 20, 15)},
		{"sample.mp4", "video/mp4", []byte("\x00\x00\x00\x14ftypmp42\x00\x00\x00\x00isom")},
		{"invalid.png", "", []byte("secret")},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := FileEntry{FileName: test.name, SavePath: "/storage/" + test.name, FileSize: int64(len(test.data))}
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("newToken") != "secret" {
					t.Error("preview authentication missing")
				}
				if r.URL.Path == "/pc_file_manager/download_info" {
					fmt.Fprintf(w, `{"totalCount":1,"totalSize":%d}`, entry.FileSize)
					return
				}
				if r.URL.Path != "/pc_file_manager/download" {
					t.Error("unexpected preview route")
				}
				archive := tar.NewWriter(w)
				archive.WriteHeader(&tar.Header{Name: test.name, Typeflag: tar.TypeReg, Mode: 0600, Size: entry.FileSize})
				archive.Write(test.data)
				archive.Close()
			})
			output := filepath.Join(t.TempDir(), test.name)
			preview, err := client.PrepareMediaPreview(context.Background(), &Session{Token: "secret"}, entry, output)
			if test.mime == "" {
				if err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatal("invalid media bytes exposed", err)
				}
				return
			}
			hash := sha256.Sum256(test.data)
			if err != nil || preview.Path != output || preview.MIME != test.mime || preview.Bytes != entry.FileSize || preview.SHA256 != hex.EncodeToString(hash[:]) {
				t.Fatal("media preview failed", err)
			}
			info, err := os.Stat(output)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("media cache is not private", err)
			}
			if _, err := client.PrepareMediaPreview(context.Background(), &Session{Token: "secret"}, entry, output); err == nil {
				t.Fatal("preview overwrote cache")
			}
		})
	}
}

func TestMediaSelectionsRejectedBeforeNetwork(t *testing.T) {
	client := fileTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unsafe selection reached phone") })
	for _, entry := range []FileEntry{
		{FileName: "sample.png", SavePath: "/sample.png", FileSize: MaxImagePreviewBytes + 1},
		{FileName: "sample.mp4", SavePath: "/sample.mp4", FileSize: MaxVideoPreviewBytes + 1},
		{FileName: "sample.svg", SavePath: "/sample.svg", FileSize: 10},
		{FileName: "sample.png", SavePath: "/sample.png", FileSize: 10, IsGroup: true},
		{FileName: "sample.png", SavePath: "/sample.png", FileSize: 10, IsDirectory: true},
	} {
		if _, err := client.PrepareMediaPreview(context.Background(), &Session{Token: "secret"}, entry, filepath.Join(t.TempDir(), "cache")); err == nil {
			t.Fatal("unsafe preview accepted")
		}
	}
	entry := FileEntry{FileName: "sample.png", SavePath: "/sample.png", FileSize: 10}
	for _, dimension := range []int{-1, 0, 1025} {
		if _, err := client.FetchThumbnail(context.Background(), &Session{Token: "secret"}, entry, dimension, 160); err == nil {
			t.Fatal("unsafe dimensions accepted")
		}
	}
	if _, _, err := imageMediaType(io.LimitReader(strings.NewReader("invalid"), 10), 4096, 16<<20); err == nil {
		t.Fatal("invalid image accepted")
	}
}
