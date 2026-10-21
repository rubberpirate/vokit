package desktop

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBridgeBoundsClipboardEventsAndIgnoresDiagnostics(t *testing.T) {
	b := &Bridge{changes: make(chan string, 16), ready: make(chan struct{})}
	var messages strings.Builder
	enc := json.NewEncoder(&messages)
	_ = enc.Encode(map[string]string{"event": "ready"})
	_ = enc.Encode(map[string]string{"event": "notification", "text": "not clipboard"})
	_ = enc.Encode(map[string]string{"event": "text", "text": strings.Repeat("x", MaxTextBytes+1)})
	_ = enc.Encode(map[string]string{"event": "text", "text": ""})
	_ = enc.Encode(map[string]string{"event": "text", "text": "Hello नमस्ते\n: \"世界\""})
	b.read(strings.NewReader(messages.String()))
	select {
	case <-b.ready:
	default:
		t.Fatal("not ready")
	}
	if len(b.changes) != 1 || <-b.changes != "Hello नमस्ते\n: \"世界\"" {
		t.Fatal("bad clipboard event")
	}
}

func TestBridgeWritesPrivateJSONAndRejectsInvalidText(t *testing.T) {
	reader, writer := io.Pipe()
	b := &Bridge{input: writer, done: make(chan struct{}), sets: make(chan acknowledgement, 1)}
	got := make(chan map[string]string, 1)
	go func() {
		var value map[string]string
		_ = json.NewDecoder(reader).Decode(&value)
		b.sets <- acknowledgement{ID: value["id"], OK: true}
		got <- value
	}()
	if err := b.Set(context.Background(), "test: नमस्ते\n"); err != nil {
		t.Fatal(err)
	}
	if value := <-got; value["op"] != "set_text" || value["text"] != "test: नमस्ते\n" || value["id"] != "1" {
		t.Fatal("bad JSON")
	}
	if b.Set(context.Background(), strings.Repeat("x", MaxTextBytes+1)) == nil {
		t.Fatal("oversized text accepted")
	}
	if b.Notify(context.Background(), "private arbitrary contents") == nil {
		t.Fatal("arbitrary notification accepted")
	}
	reader.Close()
	writer.Close()
}

func TestNotificationTimeoutCannotAcknowledgeLaterRequest(t *testing.T) {
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	var stopped atomic.Bool
	b := &Bridge{
		input: inputWriter, done: make(chan struct{}),
		cancel: func() { stopped.Store(true) },
		sets:   make(chan acknowledgement, 1), notifications: make(chan acknowledgement, 1),
	}
	readDone := make(chan struct{})
	go func() {
		b.read(outputReader)
		close(readDone)
	}()
	requests := make(chan map[string]string, 3)
	decodeDone := make(chan struct{})
	go func() {
		defer close(decodeDone)
		decoder := json.NewDecoder(inputReader)
		for {
			var value map[string]string
			if decoder.Decode(&value) != nil {
				return
			}
			requests <- value
		}
	}()
	t.Cleanup(func() {
		inputReader.Close()
		inputWriter.Close()
		outputReader.Close()
		outputWriter.Close()
		<-readDone
		<-decodeDone
	})
	receiveRequest := func() map[string]string {
		t.Helper()
		select {
		case value := <-requests:
			return value
		case <-time.After(time.Second):
			t.Fatal("desktop request missing")
			return nil
		}
	}
	receiveResult := func(results <-chan error) error {
		t.Helper()
		select {
		case err := <-results:
			return err
		case <-time.After(time.Second):
			t.Fatal("desktop result missing")
			return nil
		}
	}
	firstCtx, expireFirst := context.WithCancel(context.Background())
	defer expireFirst()
	results := make(chan error, 1)
	go func() { results <- b.Notify(firstCtx, "connected") }()
	first := receiveRequest()
	expireFirst()
	if receiveResult(results) == nil || stopped.Load() {
		t.Fatal("notification timeout succeeded or stopped clipboard helper")
	}
	go func() { results <- b.Notify(context.Background(), "disconnected") }()
	second := receiveRequest()
	if first["id"] != "1" || second["id"] != "2" {
		t.Fatal("notification IDs are not unique and increasing")
	}
	encoder := json.NewEncoder(outputWriter)
	if err := encoder.Encode(map[string]any{"event": "notification", "id": first["id"], "ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(map[string]any{"event": "notification", "id": second["id"], "ok": false}); err != nil {
		t.Fatal(err)
	}
	if err := receiveResult(results); err == nil || err.Error() != "desktop operation unavailable" {
		t.Fatal("expired notification acknowledgment completed later request")
	}
	go func() { results <- b.Set(context.Background(), "clipboard remains available") }()
	third := receiveRequest()
	if third["id"] != "3" || third["op"] != "set_text" {
		t.Fatal("clipboard operation did not share request sequence")
	}
	if err := encoder.Encode(map[string]any{"event": "set", "id": third["id"], "ok": true}); err != nil {
		t.Fatal(err)
	}
	if err := receiveResult(results); err != nil || stopped.Load() {
		t.Fatal("notification timeout prevented later clipboard operation")
	}
}

func TestClipboardSetTimeoutStopsHelper(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	stopped := make(chan struct{})
	b := &Bridge{
		input: writer, done: make(chan struct{}), sets: make(chan acknowledgement, 1),
		cancel: func() { close(stopped) },
	}
	requestRead := make(chan struct{})
	go func() {
		var value map[string]string
		_ = json.NewDecoder(reader).Decode(&value)
		close(requestRead)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- b.Set(ctx, "requires an acknowledgment") }()
	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("clipboard request missing")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("unacknowledged clipboard operation succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("clipboard timeout did not return")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("clipboard timeout did not stop helper")
	}
}

func TestReceivedFilesNotificationUsesFixedState(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	b := &Bridge{input: writer, done: make(chan struct{}), notifications: make(chan acknowledgement, 1)}
	got := make(chan map[string]string, 1)
	go func() {
		var value map[string]string
		_ = json.NewDecoder(reader).Decode(&value)
		got <- value
		b.notifications <- acknowledgement{ID: value["id"], OK: true}
	}()
	if err := b.Notify(context.Background(), "files_received"); err != nil {
		t.Fatal(err)
	}
	value := <-got
	if value["op"] != "notify" || value["state"] != "files_received" || len(value) != 3 {
		t.Fatal("notification exposed unexpected metadata")
	}
}
