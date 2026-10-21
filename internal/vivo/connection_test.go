package vivo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func connectionUploadFixture(t *testing.T) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "upload-fixture")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(32 << 20); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestFirstUseUploadEarlyPermissionRejectionStopsProducerWithoutRedial(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, 445} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			local := connectionUploadFixture(t)
			release := make(chan struct{})
			var connections atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/pc_file_manager/drop_files_info" {
					io.Copy(io.Discard, r.Body)
					fmt.Fprint(w, `{}`)
					return
				}
				if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(status)
				w.(http.Flusher).Flush()
				<-release // Keep the peer open without consuming the rejected body.
			}))
			server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					connections.Add(1)
				}
			}
			server.StartTLS()
			defer server.Close()
			defer close(release)
			client, err := newPhoneClientTrust(server.URL, "", true)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := client.UploadFile(ctx, &Session{Token: "fixture"}, local, "/storage")
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrFilePermission) {
					t.Fatal("early rejection lost permission error", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("early rejection did not stop and join the upload producer")
			}
			pin := client.CertificatePin()
			if pin == "" {
				t.Fatal("first-use certificate was not pinned")
			}
			if _, err := client.ListFiles(ctx, &Session{Token: "fixture"}, "/", 0); err == nil {
				t.Fatal("partly written request connection was reused")
			}
			if connections.Load() != 1 || client.CertificatePin() != pin {
				t.Fatal("failed upload redialed or refreshed certificate trust")
			}
		})
	}
}

func TestFirstUseUploadCancellationStopsBlockedWriterAndProducer(t *testing.T) {
	local := connectionUploadFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pc_file_manager/drop_files_info" {
			io.Copy(io.Discard, r.Body)
			fmt.Fprint(w, `{}`)
			return
		}
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)
	client, err := newPhoneClientTrust(server.URL, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.UploadFile(ctx, &Session{Token: "fixture"}, local, "/storage")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upload stream did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("upload cancellation was not propagated", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not stop and join the upload producer")
	}
}
