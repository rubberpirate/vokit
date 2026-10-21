package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	"vivolinux/internal/vivo"
)

func receivePhoneShares(ctx context.Context, options pairOptions, address netip.Addr, session *vivo.Session, incoming <-chan vivo.IncomingShare, requests chan<- []byte, emit func(vivo.Event)) {
	stage := func(name string, count int) { emit(vivo.Event{Time: time.Now().UTC(), Stage: name, Count: count}) }
	send := func(prefix string, value any) {
		data, _ := json.Marshal(value)
		select {
		case requests <- append([]byte(prefix), data...):
		case <-ctx.Done():
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case share, ok := <-incoming:
			if !ok {
				return
			}
			jobCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			success := 0
			stage("incoming_share_started", len(share.Files))
			for _, entry := range share.Files {
				if jobCtx.Err() != nil {
					break
				}
				// Each file owns a fresh authenticated data channel, as other native
				// file actions do. Never redial or refresh trust within that channel.
				var client *vivo.PhoneClient
				var err error
				if options.trustScanned {
					client, err = vivo.NewFirstUsePhoneClient(address)
				} else {
					client, err = vivo.NewPhoneClient(address, options.pin)
				}
				if err == nil {
					_, err = client.ReceiveSharedFile(jobCtx, session, entry, options.receiveDir, share.FromSharePanel)
					client.Close()
				}
				if err != nil {
					emit(vivo.Event{Time: time.Now().UTC(), Stage: "incoming_share_file_failed", Code: vivo.FileErrorCode(&vivo.FileOperationError{Phase: "receive", Err: err})})
					break
				}
				success++
				stage("incoming_share_file_received", success)
			}
			cancel()
			if success == len(share.Files) {
				send("TRANS_FILE_SUCCESS:", map[string]int{"successCount": success})
				stage("incoming_share_completed", success)
			} else {
				send("TRANS_FILE_FAIL:", map[string]int{"failCount": len(share.Files) - success, "successCount": success, "failCode": 0})
				stage("incoming_share_failed", success)
			}
		}
	}
}
