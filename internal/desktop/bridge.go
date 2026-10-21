// Package desktop owns the private pipe to the native Qt clipboard helper.
package desktop

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

const MaxTextBytes = 64 * 1024

type acknowledgement struct {
	ID string
	OK bool
}

type Bridge struct {
	cmd                 *exec.Cmd
	input               io.WriteCloser
	mu                  sync.Mutex
	changes             chan string
	done                chan struct{}
	cancel              context.CancelFunc
	ready               chan struct{}
	readyOnce           sync.Once
	operations          sync.Mutex
	nextID              uint64
	replyMu             sync.Mutex
	activeID            string
	sets, notifications chan acknowledgement
}

func Start(ctx context.Context, path string, clipboard bool) (*Bridge, error) {
	ctx, cancel := context.WithCancel(ctx)
	var args []string
	if !clipboard {
		args = []string{"--notifications-only"}
	}
	b := &Bridge{cmd: exec.CommandContext(ctx, path, args...), changes: make(chan string, 16), done: make(chan struct{}), cancel: cancel, ready: make(chan struct{}), sets: make(chan acknowledgement, 1), notifications: make(chan acknowledgement, 1)}
	var err error
	b.input, err = b.cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errors.New("cannot create desktop helper input")
	}
	output, err := b.cmd.StdoutPipe()
	if err != nil {
		cancel()
		b.input.Close()
		return nil, errors.New("cannot create desktop helper output")
	}
	// Helper diagnostics cannot include clipboard contents in CLI or logs.
	b.cmd.Stderr = io.Discard
	if err := b.cmd.Start(); err != nil {
		cancel()
		b.input.Close()
		return nil, errors.New("cannot start desktop helper; build it with make desktop")
	}
	go func() {
		defer close(b.done)
		defer close(b.changes)
		defer cancel()
		b.read(output)
		cancel()
		_ = b.cmd.Wait()
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-b.ready:
		return b, nil
	case <-b.done:
		return nil, errors.New("desktop helper exited before ready; check graphical session")
	case <-ctx.Done():
		b.Close()
		return nil, ctx.Err()
	case <-timer.C:
		b.Close()
		return nil, errors.New("desktop helper startup timed out")
	}
}

func (b *Bridge) read(input io.Reader) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	for scanner.Scan() {
		var reply struct {
			Event string `json:"event"`
			Text  string `json:"text"`
			ID    string `json:"id"`
			OK    bool   `json:"ok"`
		}
		if json.Unmarshal(scanner.Bytes(), &reply) != nil {
			return
		}
		switch reply.Event {
		case "ready":
			b.readyOnce.Do(func() { close(b.ready) })
		case "set":
			b.acknowledge(b.sets, acknowledgement{ID: reply.ID, OK: reply.OK})
		case "notification":
			b.acknowledge(b.notifications, acknowledgement{ID: reply.ID, OK: reply.OK})
		case "text":
			if !validText(reply.Text) {
				continue
			}
			select {
			case b.changes <- reply.Text:
			default:
				// Keep the latest desktop change rather than growing an unbounded queue.
				select {
				case <-b.changes:
				default:
				}
				select {
				case b.changes <- reply.Text:
				default:
				}
			}
		}
	}
}

func (b *Bridge) acknowledge(replies chan acknowledgement, reply acknowledgement) {
	b.replyMu.Lock()
	defer b.replyMu.Unlock()
	if reply.ID != b.activeID {
		return
	}
	select {
	case replies <- reply:
	default:
	}
}

func validText(text string) bool {
	return text != "" && len(text) <= MaxTextBytes && utf8.ValidString(text)
}
func (b *Bridge) Changes() <-chan string { return b.changes }

func (b *Bridge) send(ctx context.Context, value any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.done:
		return errors.New("desktop helper stopped")
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if file, ok := b.input.(*os.File); ok {
		deadline := time.Now().Add(2 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = file.SetWriteDeadline(deadline)
	}
	if json.NewEncoder(b.input).Encode(value) != nil {
		return errors.New("desktop helper write failed")
	}
	return nil
}

func (b *Bridge) Set(ctx context.Context, text string) error {
	if !validText(text) {
		return errors.New("clipboard text is empty, invalid UTF-8, or exceeds 64 KiB")
	}
	return b.request(ctx, map[string]string{"op": "set_text", "text": text}, b.sets)
}

func (b *Bridge) Notify(ctx context.Context, state string) error {
	if state != "connected" && state != "disconnected" && state != "connecting" {
		return errors.New("invalid notification state")
	}
	return b.request(ctx, map[string]string{"op": "notify", "state": state}, b.notifications)
}

func (b *Bridge) request(ctx context.Context, value map[string]string, ack <-chan acknowledgement) error {
	b.operations.Lock()
	defer b.operations.Unlock()
	b.nextID++
	id := strconv.FormatUint(b.nextID, 10)
	value["id"] = id
	b.replyMu.Lock()
	b.activeID = id
	select {
	case <-ack:
	default:
	}
	b.replyMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.send(ctx, value); err != nil {
		return err
	}
	for {
		select {
		case reply := <-ack:
			if reply.ID != id {
				continue
			}
			if !reply.OK {
				return errors.New("desktop operation unavailable")
			}
			return nil
		case <-ctx.Done():
			if ack != b.notifications {
				b.cancel()
			}
			return errors.New("desktop operation timed out")
		case <-b.done:
			return errors.New("desktop helper stopped")
		}
	}
}

func (b *Bridge) Close() { b.cancel(); <-b.done; _ = b.input.Close() }
