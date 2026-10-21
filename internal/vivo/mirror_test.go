package vivo

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestMirrorVideoNegotiatesNativeSocketsAndStripsFrameEnvelope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	keyframe := make(chan struct{})
	annexB := []byte{0, 0, 0, 1, 0x67, 0x42}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("token") != "private-token" || r.Header.Get("Sec-WebSocket-Protocol") != "v1.hc.vivo.com.cn" {
			t.Error("media token/subprotocol contract")
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v1.hc.vivo.com.cn"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		switch r.URL.Path {
		case "/mirror/control":
			_, data, err := conn.Read(ctx)
			if err != nil || !strings.HasPrefix(string(data), "COMMAND_EVENT:") {
				t.Error("keyframe not requested on control channel")
			}
			close(keyframe)
			_, _, _ = conn.Read(ctx)
		case "/mirror/screen":
			typ, data, err := conn.Read(ctx)
			if err != nil || typ != websocket.MessageText || !strings.HasPrefix(string(data), "SCREEN_START:") {
				t.Error("screen start contract")
				return
			}
			var settings map[string]any
			_ = json.Unmarshal(data[len("SCREEN_START:"):], &settings)
			if settings["no_audio"] != true || settings["support_drag"] != false || settings["mime_type"] != "video/avc" {
				t.Error("unimplemented media features advertised")
			}
			_ = conn.Write(ctx, websocket.MessageText, []byte(`DEVICE_INFO:{"mime_type":"video/avc","sessionId":0,"port":0}`))
			select {
			case <-keyframe:
			case <-ctx.Done():
				return
			}
			frame := append([]byte("FRAME:\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00"), annexB...)
			_ = conn.Write(ctx, websocket.MessageBinary, frame)
			_ = conn.Close(websocket.StatusNormalClosure, "")
		default:
			t.Error("unexpected media endpoint")
		}
	}))
	defer server.Close()
	var output bytes.Buffer
	frames := 0
	_ = mirrorVideo(ctx, &Session{Token: "private-token"}, server.URL, true, "", &output, func(e Event) {
		if e.Stage == "mirror_video_frames" {
			frames = e.Count
		}
	})
	if !bytes.Equal(output.Bytes(), annexB) || frames != 1 {
		t.Fatalf("video framing or lifecycle failed, frames=%d", frames)
	}
}

func TestInteractiveMirrorTimingAndInputUseControlChannel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ready := make(chan struct{})
	input := make(chan InputEvent, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v1.hc.vivo.com.cn"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		switch r.URL.Path {
		case "/mirror/control":
			var keyframe, clock, touch bool
			for !(keyframe && clock && touch) {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				switch {
				case bytes.HasPrefix(data, []byte("NTP:")):
					var request struct {
						T1 int64 `json:"t1"`
					}
					if json.Unmarshal(data[4:], &request) != nil || request.T1 <= 0 {
						t.Error("invalid NTP request")
						return
					}
					now := time.Now().UnixMilli()
					reply, _ := json.Marshal(map[string]int64{"t1": request.T1, "t2": now, "t3": now})
					_ = conn.Write(ctx, websocket.MessageText, append([]byte("NTP:"), reply...))
					clock = true
				case bytes.HasPrefix(data, []byte("COMMAND_EVENT:")):
					var command struct{ Action, SessionID int }
					_ = json.Unmarshal(data[len("COMMAND_EVENT:"):], &command)
					if command.Action != 6 || command.SessionID != 37 {
						t.Error("wrong negotiated keyframe session")
					}
					keyframe = true
				default:
					want, _ := EncodeInput(InputEvent{Kind: "pointer", Action: "down", Buttons: 1, X: 10, Y: 20, Width: 100, Height: 200}, 37)
					if !bytes.Equal(data, want) {
						t.Error("wrong negotiated touch session")
					}
					touch = true
				}
			}
			close(ready)
			_, _, _ = conn.Read(ctx)
		case "/mirror/screen":
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
			_ = conn.Write(ctx, websocket.MessageText, []byte(`DEVICE_INFO:{"mime_type":"video/avc","sessionId":37,"video_width":100,"video_height":200}`))
			select {
			case <-ready:
			case <-ctx.Done():
				return
			}
			frame := append([]byte("FRAME:"), make([]byte, 10)...)
			binary.BigEndian.PutUint64(frame[6:14], uint64(time.Now().UnixMilli()))
			binary.BigEndian.PutUint16(frame[14:16], 7)
			frame = append(frame, 0, 0, 0, 1, 0x65, 0x01)
			_ = conn.Write(ctx, websocket.MessageBinary, frame)
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
	}))
	defer server.Close()
	var output bytes.Buffer
	var timing *MediaTiming
	_ = mirrorVideoOptions(ctx, &Session{Token: "fixture-token"}, server.URL, true, "", &output, MirrorOptions{
		EnableTiming: true, Input: input, DeviceInfo: func(MirrorInfo) {
			input <- InputEvent{Kind: "pointer", Action: "down", Buttons: 1, X: 10, Y: 20, Width: 100, Height: 200}
		},
	}, func(e Event) {
		if e.MediaTiming != nil {
			timing = e.MediaTiming
		}
	})
	if !bytes.Equal(output.Bytes(), []byte{0, 0, 0, 1, 0x65, 0x01}) || timing == nil || timing.EncodeMS != 7 {
		t.Fatal("interactive video or derived timing was not delivered")
	}
}
