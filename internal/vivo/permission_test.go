package vivo

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFileAccessExplicitRetryRequestsPhoneBeforeUpload(t *testing.T) {
	var access FileAccess
	requests := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- access.Ensure(ctx, requests, false) }()
	select {
	case <-requests:
		access.Update(true)
	case <-ctx.Done():
		t.Fatal("no phone permission request")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := access.Ensure(ctx, requests, false); err != nil || len(requests) != 0 {
		t.Fatal("granted access re-prompted")
	}
	go func() { done <- access.Ensure(ctx, requests, true) }()
	select {
	case <-requests:
		access.Update(false)
	case <-ctx.Done():
		t.Fatal("no forced permission request")
	}
	if err := <-done; !errors.Is(err, ErrFilePermission) {
		t.Fatal("denial bypassed", err)
	}
}

func TestFileAccessCancellationDoesNotGrant(t *testing.T) {
	var access FileAccess
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(access.Ensure(ctx, make(chan struct{}), false), context.Canceled) || access.Allowed() {
		t.Fatal("cancelled access granted")
	}
}
