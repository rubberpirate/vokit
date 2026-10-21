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
	"sync"
	"testing"
	"time"
)

func TestUploadStreamsExactFileWithNewNameAndLocalHash(t *testing.T) {
	for _, content := range [][]byte{nil, bytes.Repeat([]byte("safe fixture\x00\xff"), 100000)} {
		t.Run(fmt.Sprintf("size=%d", len(content)), func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "private-original.TXT")
			if err := os.WriteFile(local, content, 0600); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var id, name string
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.Header.Get("newToken") != "secret-token" || r.Header.Get("X-ES-HTTP-VERSION") != "1" {
					t.Error("incorrect upload authentication or method")
				}
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/pc_file_manager/drop_files_info":
					id = r.URL.Query().Get("id")
					var body struct {
						ID            string `json:"id"`
						Type          string `json:"type"`
						SavePath      string `json:"savePath"`
						TotalSize     int64  `json:"totalSize"`
						TotalCount    int    `json:"totalCount"`
						DropFileItems []struct {
							FileName     string `json:"fileName"`
							FileSize     int64  `json:"fileSize"`
							IfDuplicated string `json:"ifDuplicated"`
						} `json:"dropFileItems"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.DropFileItems) != 1 {
						t.Error("invalid upload metadata")
						return
					}
					name = body.DropFileItems[0].FileName
					if len(id) != 36 || body.ID != id || body.Type != "TO_PC_FILE_MANAGER" || body.SavePath != "/storage/emulated/0/Download" || body.TotalCount != 1 || body.TotalSize != int64(len(content)) || body.DropFileItems[0].FileSize != int64(len(content)) || body.DropFileItems[0].IfDuplicated != "rename" || !strings.HasPrefix(name, "vivo-transfer-") || !strings.HasSuffix(name, ".txt") || strings.Contains(name, "private-original") {
						t.Error("incorrect upload metadata or collision policy")
					}
					fmt.Fprint(w, `{"status":0}`)
				case "/upload/drop_file_to_phone":
					if r.URL.Query().Get("id") != id || r.URL.Query().Get("type") != "tar" || r.Header.Get("Content-Type") != "application/x-tar" {
						t.Error("incorrect upload stream route")
					}
					archive := tar.NewReader(r.Body)
					header, err := archive.Next()
					if err != nil || header.Name != name || header.Typeflag != tar.TypeReg || header.Size != int64(len(content)) {
						t.Error("incorrect upload TAR header", err)
						return
					}
					got, err := io.ReadAll(archive)
					if err != nil || !bytes.Equal(got, content) {
						t.Error("uploaded bytes changed", err)
					}
					if _, err := archive.Next(); err != io.EOF {
						t.Error("unexpected TAR entry", err)
					}
					io.Copy(io.Discard, r.Body)
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"dropFileItems": []map[string]string{{"fileName": name, "savePath": "/storage/emulated/0/Download/" + name}}}})
				default:
					t.Error("unexpected upload route")
				}
			})
			result, err := client.UploadFile(context.Background(), &Session{Token: "secret-token"}, local, "/storage/emulated/0/Download")
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(content)
			if result.FileName != name || result.SavePath != "/storage/emulated/0/Download/"+name || result.Bytes != int64(len(content)) || result.LocalSHA256 != hex.EncodeToString(hash[:]) {
				t.Fatal("incorrect upload result")
			}
		})
	}
}

func TestUploadResponseVariantsAndBounds(t *testing.T) {
	for _, status := range []int{445, http.StatusForbidden} {
		_, err := readFileAcknowledgment(&http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("private-token private-name"))})
		if !errors.Is(err, ErrFilePermission) || strings.Contains(err.Error(), "private-") {
			t.Fatal("phone permission response not classified safely", err)
		}
	}
	for _, test := range []struct {
		body string
		bad  bool
	}{
		{"", false}, {`{}`, false}, {`{"status":0}`, false}, {`{"status":"0"}`, false},
		{`{"status":1}`, false}, {`{"status":null}`, false}, {`not json secret-token`, false},
		{strings.Repeat("x", maxResponseBytes+1), true},
	} {
		response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body))}
		_, err := readFileAcknowledgment(response)
		if (err != nil) != test.bad || err != nil && strings.Contains(err.Error(), "secret-token") {
			t.Fatal("unsafe upload response validation", err)
		}
	}
}

func TestUploadAcceptsHTTPAcknowledgmentWithoutJSON(t *testing.T) {
	for _, reply := range []string{"", "OK", `"OK"`, `{"status":1}`, `{"data":"OK"}`, `{"data":null}`, `{"data":42}`} {
		t.Run(reply, func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "fixture.txt")
			if err := os.WriteFile(local, []byte("fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				fmt.Fprint(w, reply)
			})
			result, err := client.UploadFile(context.Background(), &Session{Token: "secret"}, local, "/storage")
			if err != nil || result.Bytes != 7 || result.FileName == "" || result.SavePath != "" {
				t.Fatal("HTTP acknowledgment did not complete upload", err)
			}
		})
	}
}

func TestUploadRejectsLocalSymlinksDirectoriesAndLargeFiles(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "source")
	os.WriteFile(file, []byte("fixture"), 0600)
	link := filepath.Join(directory, "link")
	os.Symlink(file, link)
	large := filepath.Join(directory, "large")
	fd, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := fd.Truncate(MaxUploadBytes + 1); err != nil {
		t.Fatal(err)
	}
	fd.Close()
	client := fileTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("unsafe local source reached phone") })
	for _, local := range []string{link, directory, large, filepath.Join(directory, "missing")} {
		if _, err := client.UploadFile(context.Background(), &Session{Token: "secret"}, local, "/storage/emulated/0"); err == nil {
			t.Fatal("accepted unsafe local source")
		}
	}
}

func TestUploadRejectsUnsafePeerPath(t *testing.T) {
	local := filepath.Join(t.TempDir(), "source")
	os.WriteFile(local, []byte("fixture"), 0600)
	var name string
	client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pc_file_manager/drop_files_info" {
			var body struct {
				Items []struct{ FileName string } `json:"dropFileItems"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			name = body.Items[0].FileName
			fmt.Fprint(w, `{}`)
			return
		}
		io.Copy(io.Discard, r.Body)
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"dropFileItems": []map[string]string{{"fileName": name, "savePath": "/storage/../private/fixture"}}}})
	})
	if _, err := client.UploadFile(context.Background(), &Session{Token: "secret"}, local, "/storage"); err == nil {
		t.Fatal("accepted unsafe peer destination")
	}
}

func TestUploadCancellationAndEarlyFailureJoinProducer(t *testing.T) {
	for _, status := range []int{0, http.StatusForbidden, 445} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			local := filepath.Join(t.TempDir(), "source")
			file, _ := os.Create(local)
			file.Truncate(32 << 20)
			file.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := fileTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/pc_file_manager/drop_files_info" {
					io.Copy(io.Discard, r.Body)
					fmt.Fprint(w, `{}`)
					return
				}
				if status != 0 {
					w.WriteHeader(status)
					w.(http.Flusher).Flush()
					return
				}
				buffer := make([]byte, 512)
				r.Body.Read(buffer)
				cancel()
				io.Copy(io.Discard, r.Body)
			})
			done := make(chan error, 1)
			go func() {
				_, err := client.UploadFile(ctx, &Session{Token: "secret"}, local, "/storage")
				done <- err
			}()
			select {
			case err := <-done:
				if status == 0 && !errors.Is(err, context.Canceled) || status != 0 && !errors.Is(err, ErrFilePermission) {
					t.Fatal("unsafe cancellation/failure result", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upload failed to stop and join producer")
			}
		})
	}
}

func TestFirstUseUploadStreamsFromStableDescriptor(t *testing.T) {
	directory := t.TempDir()
	local := filepath.Join(directory, "source")
	content := bytes.Repeat([]byte("x"), 700*1024)
	if err := os.WriteFile(local, content, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pc_file_manager/drop_files_info" {
			io.Copy(io.Discard, r.Body)
			if err := os.Rename(local, filepath.Join(directory, "original")); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(local, []byte("replacement"), 0600); err != nil {
				t.Error(err)
			}
			fmt.Fprint(w, `{}`)
			return
		}
		archive := tar.NewReader(r.Body)
		if _, err := archive.Next(); err != nil {
			t.Error(err)
			return
		}
		data, err := io.ReadAll(archive)
		if err != nil || !bytes.Equal(data, content) {
			t.Error("upload reopened a replaced source path", err)
		}
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client, err := newPhoneClientTrust(server.URL, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.UploadFile(context.Background(), &Session{Token: "secret"}, local, "/storage")
	if err != nil || result.Bytes != int64(len(content)) {
		t.Fatal("first-use streaming upload failed", err)
	}
}
