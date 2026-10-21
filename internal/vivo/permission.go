package vivo

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrFilePermission = errors.New("phone file access was not granted; approve the Office Kit file prompt")

// FileAccess routes user-initiated retries through the existing control socket.
type FileAccess struct {
	mu        sync.Mutex
	requestMu sync.Mutex
	allowed   bool
	changed   chan struct{}
}

func (p *FileAccess) Allowed() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.allowed }
func (p *FileAccess) Update(allowed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.allowed = allowed
	if p.changed != nil {
		close(p.changed)
	}
	p.changed = make(chan struct{})
}

func (p *FileAccess) Ensure(ctx context.Context, requests chan<- struct{}, force bool) error {
	p.requestMu.Lock()
	defer p.requestMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	if p.allowed && !force {
		p.mu.Unlock()
		return nil
	}
	if p.changed == nil {
		p.changed = make(chan struct{})
	}
	changed := p.changed
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	select {
	case requests <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		if p.Allowed() {
			return nil
		}
		return ErrFilePermission
	}
}
