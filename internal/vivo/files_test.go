package vivo

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fileTestClient(t *testing.T, handler http.HandlerFunc) *PhoneClient {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	pin := sha256.Sum256(server.Certificate().Raw)
	client, err := newPhoneClient(server.URL, hex.EncodeToString(pin[:]))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestFileRequestsClassifyPermissionDenial(t *testing.T) {
	for _, status := range []int{445, http.StatusForbidden} {
		for _, operation := range []string{"list", "prepare", "download"} {
			t.Run(fmt.Sprintf("%s/%d", operation, status), func(t *testing.T) {
				client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					if operation == "download" && r.URL.Path == "/pc_file_manager/download_info" {
						fmt.Fprint(w, `{"totalCount":1,"totalSize":3}`)
						return
					}
					w.WriteHeader(status)
				})
				session := &Session{Token: "secret"}
				var err error
				output := filepath.Join(t.TempDir(), "new-file")
				if operation == "list" {
					_, err = client.ListFiles(context.Background(), session, "/storage", 0)
				} else {
					_, err = client.DownloadFile(context.Background(), session, FileEntry{FileName: "fixture", SavePath: "/storage/fixture", FileSize: 3}, output)
				}
				if !errors.Is(err, ErrFilePermission) {
					t.Fatal("permission rejection was not classified", err)
				}
				if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("denied download created a destination")
				}
			})
		}
	}
}

func TestListFilesContractAndMetadataValidation(t *testing.T) {
	for _, test := range []struct {
		name, response string
		wantError      bool
	}{
		{"valid", `{"dataList":[{"fileName":"fixture.txt","savePath":"/storage/emulated/0/fixture.txt","fileSize":3,"date":12,"isDirectory":false}],"totalCount":1}`, false},
		{"traversal", `{"dataList":[{"fileName":"fixture.txt","savePath":"/storage/../fixture.txt"}],"totalCount":1}`, true},
		{"remote error", `{"code":"denied"}`, true},
		{"oversized", strings.Repeat("x", maxResponseBytes+1), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/pc_file_manager/channel" || r.Header.Get("newToken") != "secret-token" || r.Header.Get("X-ES-HTTP-VERSION") != "1" {
					t.Error("incorrect listing route or authentication")
				}
				var body map[string]any
				if json.NewDecoder(r.Body).Decode(&body) != nil || body["data"] != "/storage/emulated/0" || body["pageIndex"] != float64(2) || body["pageNumber"] != float64(200) || body["type"] != "REQUEST_POSTS_FILELIST" || body["versionTag"] != float64(1) {
					t.Error("incorrect listing body")
				}
				fmt.Fprint(w, test.response)
			})
			page, err := client.ListFiles(context.Background(), &Session{Token: "secret-token"}, "/storage/emulated/0", 2)
			if (err != nil) != test.wantError {
				t.Fatalf("unexpected listing result: %v", err)
			}
			if !test.wantError && (len(page.DataList) != 1 || page.DataList[0].FileSize != 3) {
				t.Fatal("file metadata missing")
			}
		})
	}
}

func TestListFilesMapsStorageRootAndSkipsExplicitGroups(t *testing.T) {
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Data string `json:"data"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Data != "/" {
			t.Error("storage root was not mapped to browsing alias")
		}
		fmt.Fprint(w, `{"dataList":[{"isGroup":true,"groupName":"Today","groupCount":1},{"fileName":"fixture","savePath":"/storage/emulated/0/fixture","fileSize":3},{"fileName":"Phone storage","savePath":"/storage/emulated/0","isDirectory":true}],"totalCount":2}`)
	})
	page, err := client.ListFiles(context.Background(), &Session{Token: "secret", StorageRoot: "/storage/emulated/0"}, "/storage/emulated/0", 0)
	if err != nil || len(page.DataList) != 2 || page.DataList[0].FileName != "fixture" || !page.DataList[1].IsDirectory {
		t.Fatal("group filtering failed", err)
	}
}

func fileTestTar(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	for i := range headers {
		if err := writer.WriteHeader(&headers[i]); err != nil {
			t.Fatal(err)
		}
		if headers[i].Typeflag == tar.TypeReg {
			if _, err := writer.Write(bytes.Repeat([]byte("a"), int(headers[i].Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestDownloadFileContractSafetyAndIntegrity(t *testing.T) {
	entry := FileEntry{FileName: "fixture.txt", SavePath: "/storage/emulated/0/fixture.txt", FileSize: 3}
	regular := tar.Header{Name: entry.FileName, Typeflag: tar.TypeReg, Size: 3, Mode: 0600}
	for _, test := range []struct {
		name string
		tar  []byte
		bad  bool
	}{
		{"valid", fileTestTar(t, regular), false},
		{"wrong size", fileTestTar(t, tar.Header{Name: entry.FileName, Typeflag: tar.TypeReg, Size: 2}), true},
		{"extra entry", fileTestTar(t, regular, regular), true},
		{"traversal", fileTestTar(t, tar.Header{Name: "../fixture.txt", Typeflag: tar.TypeReg, Size: 3}), true},
		{"symlink", fileTestTar(t, tar.Header{Name: entry.FileName, Typeflag: tar.TypeSymlink, Linkname: "target"}), true},
		{"hardlink", fileTestTar(t, tar.Header{Name: entry.FileName, Typeflag: tar.TypeLink, Linkname: "target"}), true},
		{"device", fileTestTar(t, tar.Header{Name: entry.FileName, Typeflag: tar.TypeChar}), true},
		{"invalid", []byte("invalid archive secret-token"), true},
		{"hidden trailing data", append(fileTestTar(t, regular), []byte("hidden")...), true},
		{"truncated", fileTestTar(t, regular)[:513], true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requestID string
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("newToken") != "secret-token" || r.Header.Get("X-ES-HTTP-VERSION") != "1" {
					t.Error("missing transfer headers")
				}
				switch r.URL.Path {
				case "/pc_file_manager/download_info":
					requestID = r.URL.Query().Get("id")
					var body struct {
						DownloadList []string `json:"downloadList"`
						Type         string   `json:"type"`
						Total        int64    `json:"total"`
					}
					if r.Method != "POST" || len(requestID) != 36 || json.NewDecoder(r.Body).Decode(&body) != nil || len(body.DownloadList) != 1 || body.DownloadList[0] != entry.SavePath || body.Type != "FROM_PC_FILE_MANAGER" || body.Total != 3 {
						t.Error("incorrect preparation contract")
					}
					fmt.Fprint(w, `{"totalCount":1,"totalSize":3}`)
				case "/pc_file_manager/download":
					if r.Method != "GET" || requestID == "" || r.URL.Query().Get("id") != requestID {
						t.Error("transfer identifier changed")
					}
					w.Write(test.tar)
				default:
					t.Error("unexpected download route")
				}
			})
			dir := t.TempDir()
			output := filepath.Join(dir, "saved.txt")
			result, err := client.DownloadFile(context.Background(), &Session{Token: "secret-token"}, entry, output)
			if (err != nil) != test.bad {
				t.Fatalf("unexpected download result: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "fixture")) {
				t.Fatal("download error leaked remote data")
			}
			if test.bad {
				if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed download created destination")
				}
			} else {
				data, err := os.ReadFile(output)
				hash := sha256.Sum256([]byte("aaa"))
				if err != nil || string(data) != "aaa" || result.Bytes != 3 || result.SHA256 != hex.EncodeToString(hash[:]) {
					t.Fatal("download integrity mismatch")
				}
				info, _ := os.Stat(output)
				if info.Mode().Perm() != 0600 {
					t.Fatal("unsafe destination permissions")
				}
				os.Remove(output)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("download left staging files")
			}
		})
	}
}

func TestDownloadRefusesExistingPathsSymlinksAndUnsafeSelection(t *testing.T) {
	client := fileTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unsafe request reached phone") })
	directory := t.TempDir()
	output := filepath.Join(directory, "existing")
	if err := os.WriteFile(output, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	valid := FileEntry{FileName: "fixture", SavePath: "/storage/fixture", FileSize: 1}
	for _, test := range []struct {
		entry  FileEntry
		output string
	}{
		{valid, output},
		{valid, filepath.Join(link, "new")},
		{FileEntry{FileName: "fixture", SavePath: "/storage/../fixture"}, filepath.Join(directory, "new")},
		{FileEntry{FileName: "fixture", SavePath: "/storage/fixture", FileSize: MaxDownloadBytes + 1}, filepath.Join(directory, "new")},
		{FileEntry{FileName: "fixture", SavePath: "/storage/fixture", IsDirectory: true}, filepath.Join(directory, "new")},
	} {
		if _, err := client.DownloadFile(context.Background(), &Session{Token: "secret"}, test.entry, test.output); err == nil {
			t.Fatal("accepted unsafe download")
		}
	}
	data, _ := os.ReadFile(output)
	if string(data) != "original" {
		t.Fatal("existing destination was corrupted")
	}
}

func TestDownloadCancellationRemovesStage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entry := FileEntry{FileName: "fixture", SavePath: "/storage/fixture", FileSize: 100}
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pc_file_manager/download_info" {
			fmt.Fprint(w, `{"totalCount":1,"totalSize":100}`)
			return
		}
		var header bytes.Buffer
		writer := tar.NewWriter(&header)
		writer.WriteHeader(&tar.Header{Name: "fixture", Typeflag: tar.TypeReg, Size: 100})
		w.Write(header.Bytes())
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	})
	dir := t.TempDir()
	_, err := client.DownloadFile(ctx, &Session{Token: "secret"}, entry, filepath.Join(dir, "saved"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("canceled download left files")
	}
}

func TestDownloadChangedArchiveAndDestinationRace(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("metadataChanged=%t", changed), func(t *testing.T) {
			directory := t.TempDir()
			output := filepath.Join(directory, "saved")
			entry := FileEntry{FileName: "fixture", SavePath: "/storage/fixture", FileSize: 3}
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/pc_file_manager/download_info" {
					if changed {
						fmt.Fprint(w, `{"totalCount":1,"totalSize":4}`)
					} else {
						if err := os.WriteFile(output, []byte("original"), 0600); err != nil {
							t.Error(err)
						}
						fmt.Fprint(w, `{"totalCount":1,"totalSize":3}`)
					}
					return
				}
				if changed {
					w.Write(fileTestTar(t, tar.Header{Name: "fixture", Typeflag: tar.TypeReg, Size: 4}))
					return
				}
				w.Write(fileTestTar(t, tar.Header{Name: "fixture", Typeflag: tar.TypeReg, Size: 3}))
			})
			if _, err := client.DownloadFile(context.Background(), &Session{Token: "secret"}, entry, output); err == nil {
				t.Fatal("unsafe download committed")
			}
			if changed {
				if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("changed metadata created destination")
				}
			} else if data, _ := os.ReadFile(output); string(data) != "original" {
				t.Fatal("concurrent destination was overwritten")
			}
		})
	}
}

func TestDownloadAcceptsOptionalPreparationMetadata(t *testing.T) {
	for _, acknowledgment := range []string{"", "OK", "[]", "{}", `{"totalCount":0,"totalSize":0}`} {
		t.Run(acknowledgment, func(t *testing.T) {
			entry := FileEntry{FileName: "fixture", SavePath: "/storage/fixture", FileSize: 3}
			archive := fileTestTar(t, tar.Header{Name: entry.FileName, Typeflag: tar.TypeReg, Size: entry.FileSize})
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/pc_file_manager/download_info" {
					fmt.Fprint(w, acknowledgment)
					return
				}
				w.Write(archive)
			})
			result, err := client.DownloadFile(context.Background(), &Session{Token: "secret"}, entry, filepath.Join(t.TempDir(), "new-file"))
			if err != nil || result.Bytes != 3 {
				t.Fatal("valid archive rejected after HTTP acknowledgment", err)
			}
		})
	}
}

func TestTarPaddingBound(t *testing.T) {
	entry := FileEntry{FileName: "empty", SavePath: "/empty"}
	archive := fileTestTar(t, tar.Header{Name: "empty", Typeflag: tar.TypeReg})
	body := io.MultiReader(bytes.NewReader(archive), bytes.NewReader(make([]byte, 1<<20)))
	if _, err := readSingleFileTar(context.Background(), body, entry, io.Discard); err == nil {
		t.Fatal("unbounded padding accepted")
	}
}
