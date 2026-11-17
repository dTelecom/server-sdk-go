package cloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// multipartBody is a multipart/form-data request body.
type multipartBody struct {
	reader      io.Reader
	contentType string
}

type formField struct{ name, value string }

// memoryForm builds a small multipart body in memory: one file part, then the fields.
func memoryForm(fileField, filename string, data []byte, fields []formField) (*multipartBody, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(fileField, filename)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(data); err != nil {
		return nil, err
	}
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		if err := w.WriteField(f.name, f.value); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return &multipartBody{reader: &buf, contentType: w.FormDataContentType()}, nil
}

// streamingForm streams a large file as multipart: the fields first, then the file part.
// The HTTP client closes the pipe when the request ends, which stops the writer.
func streamingForm(fileField, filename string, file io.Reader, fields []formField) *multipartBody {
	pr, pw := io.Pipe()
	w := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for _, f := range fields {
				if f.value == "" {
					continue
				}
				if err := w.WriteField(f.name, f.value); err != nil {
					return err
				}
			}
			part, err := w.CreateFormFile(fileField, filename)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, file); err != nil {
				return err
			}
			return w.Close()
		}()
		pw.CloseWithError(err)
	}()
	return &multipartBody{reader: pr, contentType: w.FormDataContentType()}
}

type rawSpeechSession struct {
	SessionID string    `json:"session_id"`
	Token     string    `json:"token"`
	ServerURL string    `json:"server_url"`
	StreamURL string    `json:"stream_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (r rawSpeechSession) session() *SpeechSession {
	stream := r.StreamURL
	if stream == "" { // clouds before 2026-11-16 return no stream_url
		stream = strings.TrimRight(r.ServerURL, "/") + "/v1/stream"
	}
	return &SpeechSession{SessionID: r.SessionID, Token: r.Token, ServerURL: r.ServerURL, StreamURL: stream, ExpiresAt: r.ExpiresAt}
}

// STTService opens live speech-to-text sessions, prepaid by time.
type STTService struct{ http *cloudHTTP }

// CreateSession opens a session: connect to StreamURL and send Token as session_key. A key
// serves one connection; after a disconnect, open a new session.
func (s *STTService) CreateSession(ctx context.Context, opts STTSessionOptions) (*SpeechSession, error) {
	minutes, err := durationMinutes(opts.Duration)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"duration_minutes": minutes}
	if opts.Language != "" {
		body["language"] = opts.Language
	}
	if opts.WebRTCSessionID != "" {
		body["webrtc_session_id"] = opts.WebRTCSessionID
	}
	var r rawSpeechSession
	if err := s.http.json(ctx, http.MethodPost, "/v1/stt/session", body, nil, &r); err != nil {
		return nil, err
	}
	return r.session(), nil
}

// Extend adds time, rounded up to whole minutes. Send the new token to the open socket as
// {"type": "extend", "session_key": token}. After the session ended it fails with
// session_not_extendable (409).
func (s *STTService) Extend(ctx context.Context, sessionID string, additional time.Duration) (*ExtendedSTTSession, error) {
	minutes, err := durationMinutes(additional)
	if err != nil {
		return nil, err
	}
	var r struct {
		Token        string    `json:"token"`
		NewExpiresAt time.Time `json:"new_expires_at"`
	}
	body := map[string]any{"session_id": sessionID, "additional_minutes": minutes}
	if err := s.http.json(ctx, http.MethodPost, "/v1/stt/session/extend", body, nil, &r); err != nil {
		return nil, err
	}
	return &ExtendedSTTSession{Token: r.Token, ExpiresAt: r.NewExpiresAt}, nil
}

// TTSService opens live text-to-speech sessions, prepaid by characters.
type TTSService struct{ http *cloudHTTP }

// CreateSession opens a session: connect to StreamURL and send Token as session_key. A key
// serves one connection; after a disconnect, open a new session. A session lives 30 minutes.
func (s *TTSService) CreateSession(ctx context.Context, opts TTSSessionOptions) (*SpeechSession, error) {
	body := map[string]any{"max_characters": opts.MaxCharacters}
	if opts.Language != "" {
		body["language"] = opts.Language
	}
	if opts.Voice != "" {
		body["voice"] = opts.Voice
	}
	if opts.WebRTCSessionID != "" {
		body["webrtc_session_id"] = opts.WebRTCSessionID
	}
	var r rawSpeechSession
	if err := s.http.json(ctx, http.MethodPost, "/v1/tts/session", body, nil, &r); err != nil {
		return nil, err
	}
	return r.session(), nil
}

// Extend adds characters and another 30 minutes. Send the new token to the open socket as
// {"type": "extend", "session_key": token}.
func (s *TTSService) Extend(ctx context.Context, sessionID string, additionalCharacters int) (*ExtendedTTSSession, error) {
	var r struct {
		Token         string    `json:"token"`
		MaxCharacters int       `json:"max_characters"`
		NewExpiresAt  time.Time `json:"new_expires_at"`
	}
	body := map[string]any{"session_id": sessionID, "additional_characters": additionalCharacters}
	if err := s.http.json(ctx, http.MethodPost, "/v1/tts/session/extend", body, nil, &r); err != nil {
		return nil, err
	}
	return &ExtendedTTSSession{Token: r.Token, MaxCharacters: r.MaxCharacters, ExpiresAt: r.NewExpiresAt}, nil
}

// TranscriptionsService transcribes audio and video files asynchronously.
type TranscriptionsService struct{ http *cloudHTTP }

// Create uploads a file and starts a job; poll it with Get. A failed upload is not retried:
// call Create again with the same key.
func (s *TranscriptionsService) Create(ctx context.Context, opts TranscriptionOptions) (*TranscriptionJob, error) {
	if err := checkFileKey(opts.IdempotencyKey); err != nil {
		return nil, err
	}
	if opts.File == nil {
		return nil, errors.New("dtelecom cloud: File is required")
	}
	filename := opts.Filename
	if filename == "" {
		filename = "audio.media"
	}
	fields := []formField{{"language", opts.Language}}
	if opts.Diarize != nil {
		fields = append(fields, formField{"diarize", strconv.FormatBool(*opts.Diarize)})
	}
	body := streamingForm("file", filename, opts.File, fields)
	var job TranscriptionJob
	err := s.http.json(ctx, http.MethodPost, "/v1/transcriptions/jobs", body, map[string]string{"Idempotency-Key": opts.IdempotencyKey}, &job)
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// Get reads a job.
func (s *TranscriptionsService) Get(ctx context.Context, jobID string) (*TranscriptionJob, error) {
	return fileJob[TranscriptionJob](ctx, s.http, http.MethodGet, "/v1/transcriptions/jobs/", jobID)
}

// Delete cancels unfinished work, or deletes a finished result. A finished job keeps its charge.
func (s *TranscriptionsService) Delete(ctx context.Context, jobID string) (*TranscriptionJob, error) {
	return fileJob[TranscriptionJob](ctx, s.http, http.MethodDelete, "/v1/transcriptions/jobs/", jobID)
}

// WAVService synthesises text or SSML into 48 kHz mono PCM16 WAV files asynchronously.
type WAVService struct{ http *cloudHTTP }

// Create starts a job; poll it with Get and fetch the file with Download.
func (s *WAVService) Create(ctx context.Context, opts WAVOptions) (*WAVJob, error) {
	if err := checkFileKey(opts.IdempotencyKey); err != nil {
		return nil, err
	}
	body := map[string]any{"text": opts.Text}
	if opts.Voice != "" {
		body["voice"] = opts.Voice
	}
	if opts.Language != "" {
		body["language"] = opts.Language
	}
	if opts.Speed != 0 {
		body["speed"] = opts.Speed
	}
	if opts.InputFormat != "" {
		body["input_format"] = opts.InputFormat
	}
	if opts.Dictionary != nil {
		body["dictionary"] = opts.Dictionary
	}
	if opts.Watermark != nil {
		body["watermark"] = *opts.Watermark
	}
	var job WAVJob
	if err := s.http.json(ctx, http.MethodPost, "/v1/tts/jobs", body, map[string]string{"Idempotency-Key": opts.IdempotencyKey}, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Get reads a job.
func (s *WAVService) Get(ctx context.Context, jobID string) (*WAVJob, error) {
	return fileJob[WAVJob](ctx, s.http, http.MethodGet, "/v1/tts/jobs/", jobID)
}

// Download returns the finished WAV file. The caller closes it. Before the job completes it
// fails with job_not_complete (409).
func (s *WAVService) Download(ctx context.Context, jobID string) (io.ReadCloser, error) {
	id, err := segment(jobID)
	if err != nil {
		return nil, err
	}
	res, err := s.http.response(ctx, http.MethodGet, "/v1/tts/jobs/"+id+"/audio", nil, nil)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Delete cancels unfinished work, or deletes a finished file. A finished job keeps its charge.
func (s *WAVService) Delete(ctx context.Context, jobID string) (*WAVJob, error) {
	return fileJob[WAVJob](ctx, s.http, http.MethodDelete, "/v1/tts/jobs/", jobID)
}

func fileJob[T any](ctx context.Context, h *cloudHTTP, method, prefix, jobID string) (*T, error) {
	id, err := segment(jobID)
	if err != nil {
		return nil, err
	}
	var job T
	if err := h.json(ctx, method, prefix+id, nil, nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Translate translates text. Running means call again with the same key, or poll with
// GetTranslation. A failed translation is returned as an *APIError, such as
// inference_rejected (422).
func (c *Client) Translate(ctx context.Context, opts TranslateOptions) (*Translation, error) {
	if err := checkInferenceKey(opts.IdempotencyKey); err != nil {
		return nil, err
	}
	body := map[string]any{"q": opts.Q, "target": opts.Target}
	if opts.Source != "" {
		body["source"] = opts.Source
	}
	var t Translation
	if err := c.http.json(ctx, http.MethodPost, "/v1/translation", body, map[string]string{"Idempotency-Key": opts.IdempotencyKey}, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTranslation polls a translation, or reads its stored result without another charge.
func (c *Client) GetTranslation(ctx context.Context, requestID string) (*Translation, error) {
	return getRequest[TranslationResult](ctx, c.http, "/v1/translation/requests/", requestID)
}

// VisionService answers questions about images with dTelecom's own vision model.
type VisionService struct{ http *cloudHTTP }

// Snapshot answers a question about an image. Running means call again with the same key, or
// poll with Get.
func (s *VisionService) Snapshot(ctx context.Context, opts VisionOptions) (*VisionSnapshot, error) {
	if err := checkInferenceKey(opts.IdempotencyKey); err != nil {
		return nil, err
	}
	body, err := memoryForm("image", "image.media", opts.Image, []formField{
		{"question", opts.Question}, {"source", opts.Source}, {"session_id", opts.WebRTCSessionID},
	})
	if err != nil {
		return nil, err
	}
	var v VisionSnapshot
	if err := s.http.json(ctx, http.MethodPost, "/v1/vision/snapshot", body, map[string]string{"Idempotency-Key": opts.IdempotencyKey}, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Get polls a request, or reads its stored answer without another charge.
func (s *VisionService) Get(ctx context.Context, requestID string) (*VisionSnapshot, error) {
	return getRequest[VisionResult](ctx, s.http, "/v1/vision/requests/", requestID)
}

func getRequest[R any](ctx context.Context, h *cloudHTTP, prefix, requestID string) (*Request[R], error) {
	id, err := segment(requestID)
	if err != nil {
		return nil, err
	}
	var r Request[R]
	if err := h.json(ctx, http.MethodGet, prefix+id, nil, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ModerationService checks audio and video frames, per item or over a WebSocket.
type ModerationService struct{ http *cloudHTTP }

// Audio moderates an audio chunk.
func (s *ModerationService) Audio(ctx context.Context, opts ModerateAudioOptions) (*Moderation, error) {
	return s.upload(ctx, "audio", "audio", opts.Audio, opts.IdempotencyKey, []formField{
		{"language", opts.Language}, {"session_id", opts.WebRTCSessionID},
	})
}

// VideoFrame moderates one video frame.
func (s *ModerationService) VideoFrame(ctx context.Context, opts ModerateVideoFrameOptions) (*Moderation, error) {
	return s.upload(ctx, "video-frame", "image", opts.Image, opts.IdempotencyKey, []formField{
		{"session_id", opts.WebRTCSessionID},
	})
}

// Get polls a request, or reads its stored result without another charge.
func (s *ModerationService) Get(ctx context.Context, requestID string) (*Moderation, error) {
	m, err := getRequest[ModerationResult](ctx, s.http, "/v1/moderation/requests/", requestID)
	if err != nil {
		return nil, err
	}
	if err := checkModeration(m); err != nil {
		return nil, err
	}
	return m, nil
}

// StreamTicket returns a ticket for one moderation WebSocket, valid for 10 minutes; Stream gets
// one and connects. Moderation runs on your backend, which acts on the results.
func (s *ModerationService) StreamTicket(ctx context.Context) (*ModerationStreamTicket, error) {
	var r struct {
		Token           string    `json:"token"`
		WebSocketPath   string    `json:"websocket_path"`
		ExpiresAt       time.Time `json:"expires_at"`
		Protocol        string    `json:"protocol"`
		MaxInflight     int       `json:"max_inflight"`
		AudioFormat     string    `json:"audio_format"`
		SampleRate      int       `json:"sample_rate"`
		Channels        int       `json:"channels"`
		MaxAudioSeconds int       `json:"max_audio_seconds"`
	}
	if err := s.http.json(ctx, http.MethodPost, "/v1/moderation/streams", nil, nil, &r); err != nil {
		return nil, err
	}
	base, err := url.Parse(s.http.baseURL + "/")
	if err != nil {
		return nil, fmt.Errorf("dtelecom cloud: base URL: %w", err)
	}
	ws, err := base.Parse(r.WebSocketPath)
	if err != nil {
		return nil, fmt.Errorf("dtelecom cloud: websocket path: %w", err)
	}
	if ws.Scheme == "https" {
		ws.Scheme = "wss"
	} else {
		ws.Scheme = "ws"
	}
	return &ModerationStreamTicket{
		Token: r.Token, WebSocketURL: ws.String(), ExpiresAt: r.ExpiresAt, Protocol: r.Protocol,
		MaxInflight: r.MaxInflight, AudioFormat: r.AudioFormat, SampleRate: r.SampleRate,
		Channels: r.Channels, MaxAudioSeconds: r.MaxAudioSeconds,
	}, nil
}

// Stream gets a ticket and opens the moderation WebSocket with it.
func (s *ModerationService) Stream(ctx context.Context) (*ModerationStream, error) {
	ticket, err := s.StreamTicket(ctx)
	if err != nil {
		return nil, err
	}
	return ConnectModerationStream(ctx, ticket)
}

func (s *ModerationService) upload(ctx context.Context, path, field string, media []byte, key string, fields []formField) (*Moderation, error) {
	if err := checkInferenceKey(key); err != nil {
		return nil, err
	}
	body, err := memoryForm(field, "media.bin", media, fields)
	if err != nil {
		return nil, err
	}
	var m Moderation
	if err := s.http.json(ctx, http.MethodPost, "/v1/moderation/"+path, body, map[string]string{"Idempotency-Key": key}, &m); err != nil {
		return nil, err
	}
	if err := checkModeration(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// checkModeration rejects anything that is not a moderation request.
func checkModeration(m *Moderation) error {
	if m.ID == "" || (m.Kind != "moderation-audio" && m.Kind != "moderation-video") ||
		(m.Status != RequestRunning && m.Status != RequestCompleted && m.Status != RequestFailed) ||
		m.ChargedMicrocredits == "" {
		return errors.New("dtelecom cloud: invalid moderation response")
	}
	return nil
}
