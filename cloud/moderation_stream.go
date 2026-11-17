package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	moderationProtocol       = "dtelecom.moderation.v1"
	moderationConnectTimeout = 10 * time.Second
	moderationReplyTimeout   = 35 * time.Second
	moderationMaxAudioBytes  = 960_000 // 30 s of 16 kHz PCM16
	moderationMaxImageBytes  = 6 * 1024 * 1024
)

var (
	// ErrModerationClosed means the moderation WebSocket is closed; open a new stream.
	ErrModerationClosed = errors.New("dtelecom cloud: moderation connection closed")
	// ErrModerationBusy means a result is still pending: the stream takes one item at a time.
	ErrModerationBusy = errors.New("dtelecom cloud: await the previous moderation result before sending")
)

// ModerationStream is a moderation WebSocket: send audio chunks or video frames one at a time
// and get each result. Each item is billed like the HTTP call. There is no capture, sampling or
// retry. Methods are safe for concurrent use, but only one item is in flight at a time.
type ModerationStream struct {
	conn   *websocket.Conn
	busy   sync.Mutex
	mu     sync.Mutex
	closed bool
}

// ConnectModerationStream opens a stream with a ticket from Moderation.StreamTicket.
func ConnectModerationStream(ctx context.Context, ticket *ModerationStreamTicket) (*ModerationStream, error) {
	dialCtx, cancel := context.WithTimeout(ctx, moderationConnectTimeout)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, ticket.WebSocketURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dtelecom cloud: moderation connection error: %w", err)
	}
	deadline := time.Now().Add(moderationConnectTimeout)
	if d, ok := dialCtx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetWriteDeadline(deadline)
	_ = conn.SetReadDeadline(deadline)
	fail := func(err error) (*ModerationStream, error) {
		conn.Close()
		return nil, err
	}
	if err := conn.WriteJSON(map[string]string{"type": "auth", "token": ticket.Token}); err != nil {
		return fail(fmt.Errorf("dtelecom cloud: moderation connection error: %w", err))
	}
	var hello struct {
		Type     string `json:"type"`
		Protocol string `json:"protocol"`
		Error    string `json:"error"`
	}
	if err := readJSON(conn, &hello); err != nil {
		return fail(err)
	}
	if hello.Type == "error" {
		code := hello.Error
		if code == "" {
			code = "error"
		}
		return fail(fmt.Errorf("dtelecom cloud: moderation %s", code))
	}
	if hello.Type != "ready" || hello.Protocol != moderationProtocol {
		return fail(errors.New("dtelecom cloud: invalid moderation handshake"))
	}
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
	return &ModerationStream{conn: conn}, nil
}

// SendAudio moderates mono PCM16 little-endian audio at 16 kHz, up to 30 seconds. language
// defaults to en.
func (s *ModerationStream) SendAudio(ctx context.Context, idempotencyKey string, pcm []byte, language string) (*Moderation, error) {
	if language == "" {
		language = "en"
	}
	return s.send(ctx, idempotencyKey, "audio", pcm, language)
}

// SendVideoFrame moderates one image, up to 6 MiB.
func (s *ModerationStream) SendVideoFrame(ctx context.Context, idempotencyKey string, image []byte) (*Moderation, error) {
	return s.send(ctx, idempotencyKey, "video", image, "")
}

// Close closes the stream.
func (s *ModerationStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.conn.Close()
}

func (s *ModerationStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *ModerationStream) send(ctx context.Context, id, kind string, media []byte, language string) (*Moderation, error) {
	if s.isClosed() {
		return nil, ErrModerationClosed
	}
	if !s.busy.TryLock() {
		return nil, ErrModerationBusy
	}
	defer s.busy.Unlock()
	if err := checkInferenceKey(id); err != nil {
		return nil, err
	}
	limit := moderationMaxImageBytes
	if kind == "audio" {
		limit = moderationMaxAudioBytes
	}
	if len(media) == 0 || len(media) > limit || (kind == "audio" && len(media)%2 != 0) {
		return nil, errors.New("dtelecom cloud: invalid moderation media")
	}

	deadline := time.Now().Add(moderationReplyTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	// A cancelled context closes the stream: its pending reply could no longer be matched.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-stop:
		}
	}()

	header := map[string]string{"id": id, "kind": kind}
	if language != "" {
		header["language"] = language
	}
	_ = s.conn.SetWriteDeadline(deadline)
	if err := s.conn.WriteJSON(header); err != nil {
		return nil, s.fail(errors.New("dtelecom cloud: moderation send failed"))
	}
	if err := s.conn.WriteMessage(websocket.BinaryMessage, media); err != nil {
		return nil, s.fail(errors.New("dtelecom cloud: moderation send failed"))
	}

	_ = s.conn.SetReadDeadline(deadline)
	var reply struct {
		Type       string          `json:"type"`
		ID         string          `json:"id"`
		Request    json.RawMessage `json:"request"`
		Error      string          `json:"error"`
		HTTPStatus int             `json:"http_status"`
	}
	if err := readJSON(s.conn, &reply); err != nil {
		if ctx.Err() != nil {
			return nil, s.fail(ctx.Err())
		}
		return nil, s.fail(err)
	}
	if reply.ID != id || (reply.Type != "result" && reply.Type != "error") {
		return nil, s.fail(errors.New("dtelecom cloud: unexpected moderation message"))
	}
	if reply.Type == "error" {
		status, code := reply.HTTPStatus, reply.Error
		if status == 0 {
			status = 503
		}
		if code == "" {
			code = "moderation_failed"
		}
		return nil, &APIError{Status: status, Code: code, ID: reply.ID}
	}
	var m Moderation
	if err := json.Unmarshal(reply.Request, &m); err != nil {
		return nil, s.fail(errors.New("dtelecom cloud: invalid moderation response"))
	}
	if err := checkModeration(&m); err != nil {
		return nil, s.fail(err)
	}
	return &m, nil
}

// fail closes the stream and returns err.
func (s *ModerationStream) fail(err error) error {
	s.Close()
	return err
}

// readJSON reads one text message as JSON.
func readJSON(conn *websocket.Conn, out any) error {
	kind, data, err := conn.ReadMessage()
	if err != nil {
		var netErr interface{ Timeout() bool }
		if errors.As(err, &netErr) && netErr.Timeout() {
			return errors.New("dtelecom cloud: moderation response timeout")
		}
		return ErrModerationClosed
	}
	if kind != websocket.TextMessage || json.Unmarshal(data, out) != nil {
		return errors.New("dtelecom cloud: invalid moderation protocol")
	}
	return nil
}
