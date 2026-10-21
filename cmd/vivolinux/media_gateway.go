package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"vivolinux/internal/vivo"
)

type mediaOpener func(context.Context, vivo.FileEntry, string) (*http.Response, error)
type mediaTicket struct {
	entry   vivo.FileEntry
	mime    string
	expires time.Time
	ctx     context.Context
	cancel  context.CancelFunc
}

type mediaGateway struct {
	ctx      context.Context
	listener net.Listener
	server   *http.Server
	open     mediaOpener
	allowed  func() bool
	mu       sync.Mutex
	tickets  map[string]*mediaTicket
	slots    chan struct{}
}

func newMediaGateway(ctx context.Context, open mediaOpener, allowed func() bool) (*mediaGateway, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	gateway := &mediaGateway{ctx: ctx, listener: listener, open: open, allowed: allowed, tickets: make(map[string]*mediaTicket), slots: make(chan struct{}, 4)}
	gateway.server = &http.Server{Handler: gateway, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	go gateway.server.Serve(listener)
	return gateway, nil
}

func (g *mediaGateway) Close() {
	g.mu.Lock()
	for key, ticket := range g.tickets {
		ticket.cancel()
		delete(g.tickets, key)
	}
	g.mu.Unlock()
	g.server.Close()
}

func (g *mediaGateway) register(entry vivo.FileEntry) (any, error) {
	contentType, err := vivo.StreamMediaType(entry)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, ticket := range g.tickets {
		if time.Now().After(ticket.expires) {
			ticket.cancel()
			delete(g.tickets, key)
		}
	}
	if g.ctx.Err() != nil || !g.allowed() {
		return nil, errors.New("phone media permission unavailable")
	}
	if len(g.tickets) >= 32 {
		return nil, errors.New("close an existing media viewer first")
	}
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, err
	}
	key := hex.EncodeToString(keyBytes)
	ctx, cancel := context.WithCancel(g.ctx)
	g.tickets[key] = &mediaTicket{entry: entry, mime: contentType, expires: time.Now().Add(12 * time.Hour), ctx: ctx, cancel: cancel}
	return map[string]any{"url": "http://" + g.listener.Addr().String() + "/preview/" + key, "mime": contentType, "bytes": entry.FileSize}, nil
}

// A single byte range is enough for native video probing and seeking. Never
// accept multipart ranges or forward arbitrary local HTTP headers to the phone.
func mediaRange(value string, size int64) (start, end int64, err error) {
	start, end = 0, size-1
	if value == "" {
		return
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return 0, 0, errors.New("invalid range")
	}
	a, b, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok {
		return 0, 0, errors.New("invalid range")
	}
	if a == "" {
		n, e := strconv.ParseInt(b, 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, errors.New("invalid suffix range")
		}
		if n < size {
			start = size - n
		}
	} else {
		start, err = strconv.ParseInt(a, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, errors.New("invalid range start")
		}
		if b != "" {
			end, err = strconv.ParseInt(b, 10, 64)
			if err != nil || end < start {
				return 0, 0, errors.New("invalid range end")
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	return
}

func (g *mediaGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Opaque capability URLs are obtained over the private native RPC socket.
	if r.URL.RawQuery != "" || !strings.HasPrefix(r.URL.Path, "/preview/") {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/preview/")
	g.mu.Lock()
	ticket := g.tickets[key]
	if ticket != nil && (time.Now().After(ticket.expires) || ticket.ctx.Err() != nil) {
		ticket.cancel()
		delete(g.tickets, key)
		ticket = nil
	}
	if ticket != nil && r.Method == http.MethodDelete {
		ticket.cancel()
		delete(g.tickets, key)
	}
	g.mu.Unlock()
	if ticket == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !g.allowed() {
		http.Error(w, "Phone file access unavailable", http.StatusForbidden)
		return
	}
	start, end, err := mediaRange(r.Header.Get("Range"), ticket.entry.FileSize)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", ticket.entry.FileSize))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	var body io.ReadCloser
	if r.Method == http.MethodGet {
		select {
		case g.slots <- struct{}{}:
			defer func() { <-g.slots }()
		default:
			http.Error(w, "Media busy", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Hour)
		defer cancel()
		stop := context.AfterFunc(ticket.ctx, cancel)
		defer stop()
		requestedRange := ""
		if r.Header.Get("Range") != "" {
			requestedRange = fmt.Sprintf("bytes=%d-%d", start, end)
		}
		response, openErr := g.open(ctx, ticket.entry, requestedRange)
		if openErr != nil {
			http.Error(w, "Phone media stream unavailable", http.StatusBadGateway)
			return
		}
		body = response.Body
		defer body.Close()
		if response.StatusCode == http.StatusPartialContent {
			expected := fmt.Sprintf("bytes %d-%d/%d", start, end, ticket.entry.FileSize)
			if response.Header.Get("Content-Range") != expected || response.ContentLength >= 0 && response.ContentLength != end-start+1 {
				http.Error(w, "Invalid phone media range", http.StatusBadGateway)
				return
			}
		} else if response.StatusCode == http.StatusOK {
			if response.ContentLength >= 0 && response.ContentLength != ticket.entry.FileSize {
				http.Error(w, "Phone media changed", http.StatusBadGateway)
				return
			}
			// Some phone builds ignore Range. Skip in bounded memory; never cache a
			// complete file or use the file-transfer/TAR endpoint as a fallback.
			if start > 0 {
				if _, err = io.CopyN(io.Discard, body, start); err != nil {
					http.Error(w, "Phone media seek failed", http.StatusBadGateway)
					return
				}
			}
		} else {
			http.Error(w, "Phone media unavailable", http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", ticket.mime)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if r.Header.Get("Range") != "" {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, ticket.entry.FileSize))
		w.WriteHeader(http.StatusPartialContent)
	}
	if body != nil {
		_, _ = io.CopyN(w, body, end-start+1)
	}
}
