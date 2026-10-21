package vivo

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestIncomingShareWireAndValidation(t *testing.T) {
	for _, prefix := range []string{"SHARE_TRANS_FILE:", "FILE_TRANS_TAG:"} {
		share, matched, err := ParseIncomingShare([]byte(prefix + `[{"path":"/storage/emulated/0/fixture.txt","fileName":"fixture.txt","fileSize":3,"isDirectory":false}]`))
		if err != nil || !matched || len(share.Files) != 1 || share.FromSharePanel != (prefix == "SHARE_TRANS_FILE:") {
			t.Fatal("wire metadata rejected", err)
		}
	}
	for _, body := range []string{`[]`, `{}`, `[{"path":"/x/../fixture","fileName":"fixture","fileSize":3}]`, `[{"path":"/x/fixture","fileName":"../fixture","fileSize":3}]`, `[{"path":"/x/fixture","fileName":"fixture","fileSize":-1}]`, `[{"path":"/x/fixture","fileName":"fixture","fileSize":3,"isDirectory":true}]`, strings.Repeat("x", 256*1024)} {
		if _, matched, err := ParseIncomingShare([]byte("SHARE_TRANS_FILE:" + body)); !matched || err == nil {
			t.Fatal("unsafe share accepted")
		}
	}
	if _, matched, err := ParseIncomingShare([]byte("HEARTBEAT:{}")); matched || err != nil {
		t.Fatal("unrelated message matched")
	}
}

func TestReceiveDirectoryRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if PrepareReceiveDirectory(filepath.Join(root, "link", "child")) == nil {
		t.Fatal("followed symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "child")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created directory outside selected tree")
	}
	directory := filepath.Join(root, "received", "nested")
	if err := PrepareReceiveDirectory(directory); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(directory)
	if info.Mode().Perm() != 0700 {
		t.Fatal("receive directory is not private")
	}
}

func TestReceiveSharedFileConcurrentNonoverwrite(t *testing.T) {
	archive := fileTestTar(t, tar.Header{Name: "fixture.txt", Typeflag: tar.TypeReg, Size: 3, Mode: 0600})
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("newToken") != "test-token" {
			t.Error("missing auth")
		}
		if r.URL.Path == "/pc_file_manager/download_info" {
			var value struct {
				Type string `json:"type"`
			}
			if json.NewDecoder(r.Body).Decode(&value) != nil || value.Type != "FROM_VIVO_SHARE_FILE" {
				t.Error("share type not preserved")
			}
			return
		}
		if r.URL.Path != "/pc_file_manager/download" {
			t.Error("unexpected endpoint")
		}
		w.Write(archive)
	})
	directory := t.TempDir()
	original := filepath.Join(directory, "fixture.txt")
	os.WriteFile(original, []byte("keep"), 0600)
	var group sync.WaitGroup
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := client.ReceiveSharedFile(context.Background(), &Session{Token: "test-token"}, FileEntry{FileName: "fixture.txt", SavePath: "/storage/fixture.txt", FileSize: 3}, directory, true); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	data, _ := os.ReadFile(original)
	if string(data) != "keep" {
		t.Fatal("original replaced")
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 5 {
		t.Fatalf("want 5 files, got %d", len(entries))
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".vivo-") {
			t.Fatal("staging file leaked")
		}
	}
}

func TestIncomingSharePermissionAndCancellationLeaveNoFile(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "permission", true: "cancelled"}[cancelled], func(t *testing.T) {
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) })
			directory := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelled {
				cancel()
			}
			_, err := client.ReceiveSharedFile(ctx, &Session{Token: "test-token"}, FileEntry{FileName: "fixture.txt", SavePath: "/storage/fixture.txt", FileSize: 3}, directory, true)
			if err == nil || (!cancelled && !errors.Is(err, ErrFilePermission)) {
				t.Fatal("expected denied or cancelled transfer", err)
			}
			entries, _ := os.ReadDir(directory)
			if len(entries) != 0 {
				t.Fatal("incomplete transfer left a file")
			}
		})
	}
}

func TestReceiveSharedFilePreservesFirstName(t *testing.T) {
	archive := fileTestTar(t, tar.Header{Name: "fixture.txt", Typeflag: tar.TypeReg, Size: 3, Mode: 0600})
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pc_file_manager/download" {
			w.Write(archive)
		}
	})
	directory := t.TempDir()
	entry := FileEntry{FileName: "fixture.txt", SavePath: "/storage/fixture.txt", FileSize: 3}
	for range 2 {
		if _, err := client.ReceiveSharedFile(context.Background(), &Session{Token: "test-token"}, entry, directory, true); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(directory, "fixture.txt"))
	if err != nil || string(data) != "aaa" {
		t.Fatal("original filename was not preserved", err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 2 {
		t.Fatal("duplicate replaced original")
	}
}
