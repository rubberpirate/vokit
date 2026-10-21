package main

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os/exec"

	"vivolinux/internal/vivo"
)

func runNativeMirror(ctx context.Context, session *vivo.Session, phone netip.Addr, firstUse bool, pin string, event func(vivo.Event)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	player := exec.CommandContext(ctx, "ffplay", "-hide_banner", "-loglevel", "error", "-window_title", "Vivo native video preview", "-f", "h264", "-fflags", "nobuffer", "-flags", "low_delay", "-framedrop", "-an", "-i", "pipe:0")
	input, err := player.StdinPipe()
	if err != nil {
		return errors.New("cannot create video preview input")
	}
	player.Stdout, player.Stderr = io.Discard, io.Discard
	if err := player.Start(); err != nil {
		input.Close()
		return errors.New("cannot start video preview")
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = player.Wait(); cancel() }()
	err = vivo.MirrorVideo(ctx, session, phone, firstUse, pin, input, event)
	cancel()
	input.Close()
	<-done
	return err
}
