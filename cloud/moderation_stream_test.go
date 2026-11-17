package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeModerationServer speaks the moderation protocol: it checks the auth message, then answers
// each item with reply(header, media).
func fakeModerationServer(t *testing.T, hello string, reply func(header map[string]any, media []byte) any) *ModerationStreamTicket {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var auth map[string]string
		if err := conn.ReadJSON(&auth); err != nil || auth["type"] != "auth" || auth["token"] != "tk" {
			_ = conn.WriteJSON(map[string]string{"type": "error", "error": "invalid_protocol_or_authentication"})
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(hello))
		for {
			var header map[string]any
			if err := conn.ReadJSON(&header); err != nil {
				return
			}
			_, media, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if out := reply(header, media); out != nil {
				_ = conn.WriteJSON(out)
			}
		}
	}))
	t.Cleanup(srv.Close)
	return &ModerationStreamTicket{Token: "tk", WebSocketURL: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

const readyHello = `{"type":"ready","protocol":"dtelecom.moderation.v1"}`

func TestModerationStream(t *testing.T) {
	ticket := fakeModerationServer(t, readyHello, func(h map[string]any, media []byte) any {
		if h["kind"] == "video" {
			return map[string]any{"type": "error", "id": h["id"], "error": "inference_rejected", "http_status": 422}
		}
		return map[string]any{"type": "result", "id": h["id"], "request": map[string]any{
			"id": "m1", "kind": "moderation-audio", "status": "completed", "charged_microcredits": "40",
			"result": map[string]any{"decision": "pass", "language": h["language"], "usage": map[string]any{"audio_samples": len(media) / 2}},
		}}
	})
	s, err := ConnectModerationStream(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	m, err := s.SendAudio(context.Background(), "a_1", make([]byte, 3200), "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Result.Decision != "pass" || m.Result.Language != "en" || m.Result.Usage.AudioSamples != 1600 {
		t.Fatalf("result %+v", m.Result)
	}
	_, err = s.SendVideoFrame(context.Background(), "v_1", []byte("jpeg"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 422 || apiErr.Code != "inference_rejected" || apiErr.ID != "v_1" {
		t.Fatalf("want APIError, got %v", err)
	}
	// An item error keeps the stream open.
	if _, err := s.SendAudio(context.Background(), "a_2", make([]byte, 2), "de"); err != nil {
		t.Fatalf("after an item error: %v", err)
	}
	if _, err := s.SendAudio(context.Background(), "a_3", make([]byte, 3), ""); err == nil {
		t.Fatal("odd PCM length accepted")
	}
	if _, err := s.SendAudio(context.Background(), "a 4", make([]byte, 2), ""); err == nil {
		t.Fatal("bad key accepted")
	}
	s.Close()
	if _, err := s.SendAudio(context.Background(), "a_5", make([]byte, 2), ""); !errors.Is(err, ErrModerationClosed) {
		t.Fatalf("want ErrModerationClosed, got %v", err)
	}
}

func TestModerationStreamHandshake(t *testing.T) {
	ticket := fakeModerationServer(t, `{"type":"ready","protocol":"other"}`, nil)
	if _, err := ConnectModerationStream(context.Background(), ticket); err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("want handshake error, got %v", err)
	}
	bad := *ticket
	bad.Token = "wrong"
	if _, err := ConnectModerationStream(context.Background(), &bad); err == nil || !strings.Contains(err.Error(), "invalid_protocol_or_authentication") {
		t.Fatalf("want auth error, got %v", err)
	}
}

func TestModerationStreamMismatchedReplyCloses(t *testing.T) {
	ticket := fakeModerationServer(t, readyHello, func(h map[string]any, media []byte) any {
		return map[string]any{"type": "result", "id": "someone-else"}
	})
	s, err := ConnectModerationStream(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendVideoFrame(context.Background(), "v_1", []byte("x")); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("want unexpected message, got %v", err)
	}
	if _, err := s.SendVideoFrame(context.Background(), "v_2", []byte("x")); !errors.Is(err, ErrModerationClosed) {
		t.Fatalf("stream should be closed, got %v", err)
	}
}

func TestModerationStreamContextCancel(t *testing.T) {
	ticket := fakeModerationServer(t, readyHello, func(map[string]any, []byte) any { return nil }) // never answers
	s, err := ConnectModerationStream(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.SendVideoFrame(ctx, "v_1", []byte("x")); err == nil {
		t.Fatal("no error after cancel")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel did not stop the wait")
	}
	if _, err := s.SendVideoFrame(context.Background(), "v_2", []byte("x")); !errors.Is(err, ErrModerationClosed) {
		t.Fatalf("stream should be closed, got %v", err)
	}
}
