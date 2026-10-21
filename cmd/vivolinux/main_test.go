package main

import (
	"context"
	"strings"
	"testing"
)

type noRunner struct{ calls int }

func (r *noRunner) Run(context.Context, string, ...string) ([]byte, error) {
	r.calls++
	return nil, nil
}
func (r *noRunner) Stream(context.Context, string, ...string) error { r.calls++; return nil }

func TestCLIRejectsInvalidArgumentsWithoutPhoneActions(t *testing.T) {
	for _, args := range [][]string{
		{"devices", "--timeout", "0s"}, {"devices", "--timeout", "25h"},
		{"push", "local", "/sdcard/file"}, {"pull", "--serial", "abc", "/sdcard/file"},
		{"mirror"}, {"camera", "--serial", "bad;id"}, {"doctor", "unexpected"}, {"not-a-command"},
	} {
		r := &noRunner{}
		if err := run(context.Background(), args, r); err == nil || r.calls != 0 {
			t.Errorf("accepted %#v, calls=%d, error=%v", args, r.calls, err)
		}
	}
}

func TestHelpDescribesLimits(t *testing.T) {
	for _, phrase := range []string{"not a complete vivo Office Kit port", "may overwrite", "USB debugging", "not contain '..'", "virtual webcam"} {
		if !strings.Contains(usage, phrase) {
			t.Errorf("missing help limitation: %s", phrase)
		}
	}
}
