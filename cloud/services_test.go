package cloud

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSpeechSessions(t *testing.T) {
	f, c := newFakeCloud(t, 200, `{"session_id":"s","token":"k","server_url":"wss://stt.example","stream_url":"wss://stt.example/v1/stream","expires_at":"2026-11-17T10:00:00Z"}`, nil)
	s, err := c.STT.CreateSession(context.Background(), STTSessionOptions{Duration: 10 * time.Minute, Language: "en", WebRTCSessionID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/v1/stt/session" || f.body["duration_minutes"] != float64(10) || f.body["language"] != "en" || f.body["webrtc_session_id"] != "w1" {
		t.Fatalf("stt %s %v", f.path, f.body)
	}
	if s.StreamURL != "wss://stt.example/v1/stream" || s.Token != "k" {
		t.Fatalf("session %+v", s)
	}

	// An older cloud without stream_url: the SDK builds it.
	_, c2 := newFakeCloud(t, 200, `{"session_id":"s","token":"k","server_url":"wss://tts.example/","expires_at":"2026-11-17T10:00:00Z"}`, nil)
	s2, err := c2.TTS.CreateSession(context.Background(), TTSSessionOptions{MaxCharacters: 500})
	if err != nil {
		t.Fatal(err)
	}
	if s2.StreamURL != "wss://tts.example/v1/stream" {
		t.Fatalf("fallback %q", s2.StreamURL)
	}

	f3, c3 := newFakeCloud(t, 200, `{"token":"k2","max_characters":1500,"new_expires_at":"2026-11-17T10:30:00Z"}`, nil)
	e, err := c3.TTS.Extend(context.Background(), "s", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if f3.path != "/v1/tts/session/extend" || f3.body["additional_characters"] != float64(1000) || e.MaxCharacters != 1500 {
		t.Fatalf("tts extend %v %+v", f3.body, e)
	}
	if _, err := c.STT.CreateSession(context.Background(), STTSessionOptions{}); err == nil {
		t.Fatal("zero duration accepted")
	}
}

// readForm parses the multipart body the fake cloud recorded.
func readForm(t *testing.T, f *fakeCloud) (map[string]string, map[string][]byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(f.headers.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	r := multipart.NewReader(bytes.NewReader(f.raw), params["boundary"])
	fields, files := map[string]string{}, map[string][]byte{}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(p)
		if p.FileName() != "" {
			files[p.FormName()] = data
		} else {
			fields[p.FormName()] = string(data)
		}
	}
	return fields, files
}

func TestTranscriptionCreateStreamsFile(t *testing.T) {
	f, c := newFakeCloud(t, 202, `{"id":"j1","status":"uploading"}`, nil)
	job, err := c.Transcriptions.Create(context.Background(), TranscriptionOptions{
		File: strings.NewReader("RIFFdata"), Filename: "a.wav", IdempotencyKey: "key:1", Language: "de", Diarize: Bool(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/v1/transcriptions/jobs" || f.headers.Get("Idempotency-Key") != "key:1" || job.ID != "j1" || job.Status != JobUploading {
		t.Fatalf("create %s %q %+v", f.path, f.headers.Get("Idempotency-Key"), job)
	}
	fields, files := readForm(t, f)
	if string(files["file"]) != "RIFFdata" || fields["language"] != "de" || fields["diarize"] != "false" {
		t.Fatalf("form %v %q", fields, files["file"])
	}
	if _, err := c.Transcriptions.Create(context.Background(), TranscriptionOptions{File: strings.NewReader("x"), IdempotencyKey: "has space"}); err == nil {
		t.Fatal("bad file key accepted")
	}
}

func TestTranscriptionResult(t *testing.T) {
	_, c := newFakeCloud(t, 200, `{"id":"j1","status":"completed","duration_seconds":2.5,"result":{"text":"hi","language":"en",
		"segments":[{"start":0,"end":1,"speaker":"speaker_1","text":"hi"}],"media":{"sample_count":40000,"sample_rate":16000,"channels":1,"duration_seconds":2.5},
		"speaker_segments":[{"start":0,"end":1,"speaker":"speaker_1"}],"speaker_labels":"anonymous and file-local","timing_type":"segment"}}`, nil)
	job, err := c.Transcriptions.Get(context.Background(), "j1")
	if err != nil {
		t.Fatal(err)
	}
	r := job.Result
	if r == nil || r.Text != "hi" || r.Media.SampleRate != 16000 || len(r.SpeakerSegments) != 1 || r.SpeakerSegments[0].Speaker != "speaker_1" ||
		*r.Segments[0].Speaker != "speaker_1" || r.SpeakerLabels == "" || *job.DurationSeconds != 2.5 {
		t.Fatalf("result %+v", job)
	}
	if _, err := c.Transcriptions.Get(context.Background(), ""); err == nil {
		t.Fatal("empty id accepted")
	}
}

func TestWAV(t *testing.T) {
	f, c := newFakeCloud(t, 202, `{"id":"w1","status":"queued","characters":5}`, nil)
	job, err := c.WAV.Create(context.Background(), WAVOptions{Text: "<speak>hi</speak>", IdempotencyKey: "k", InputFormat: "ssml", Watermark: Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/v1/tts/jobs" || f.body["input_format"] != "ssml" || f.body["watermark"] != true || *job.Characters != 5 {
		t.Fatalf("wav %v %+v", f.body, job)
	}
	if _, ok := f.body["speed"]; ok {
		t.Fatal("zero speed sent")
	}

	f2, c2 := newFakeCloud(t, 200, "RIFF....WAVE", nil)
	rc, err := c2.WAV.Download(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	rc.Close()
	if f2.path != "/v1/tts/jobs/w1/audio" || string(data) != "RIFF....WAVE" {
		t.Fatalf("download %s %q", f2.path, data)
	}

	_, c3 := newFakeCloud(t, 409, `{"error":"job_not_complete"}`, nil)
	if _, err := c3.WAV.Download(context.Background(), "w1"); !IsCode(err, "job_not_complete") {
		t.Fatalf("want job_not_complete, got %v", err)
	}
}

func TestTranslate(t *testing.T) {
	f, c := newFakeCloud(t, 200, `{"id":"t1","kind":"translation","status":"completed","charged_microcredits":"130",
		"result":{"translated_text":"Bonjour","source":"auto","target":"fr","detected_language":{"language":"en","confidence":1},"usage":{"characters":5,"output_tokens":3}}}`, nil)
	tr, err := c.Translate(context.Background(), TranslateOptions{Q: "Hello", Target: "fr", IdempotencyKey: "tr_1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/v1/translation" || f.headers.Get("Idempotency-Key") != "tr_1" || f.body["q"] != "Hello" {
		t.Fatalf("translate %s %v", f.path, f.body)
	}
	if _, ok := f.body["source"]; ok {
		t.Fatal("empty source sent")
	}
	if tr.Status != RequestCompleted || tr.ChargedMicrocredits != "130" || tr.Result.TranslatedText != "Bonjour" ||
		tr.Result.DetectedLanguage.Language != "en" || tr.Result.Usage.Characters != 5 {
		t.Fatalf("translation %+v", tr)
	}
	if _, err := c.Translate(context.Background(), TranslateOptions{Q: "x", Target: "fr", IdempotencyKey: "key:1"}); err == nil {
		t.Fatal("file-style key accepted for inference")
	}
}

func TestVisionAndModeration(t *testing.T) {
	f, c := newFakeCloud(t, 202, `{"id":"v1","kind":"vision","status":"running","charged_microcredits":"0"}`, map[string]string{"Retry-After": "1"})
	v, err := c.Vision.Snapshot(context.Background(), VisionOptions{Image: []byte("png"), IdempotencyKey: "v_1", Question: "what?", WebRTCSessionID: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	fields, files := readForm(t, f)
	if f.path != "/v1/vision/snapshot" || string(files["image"]) != "png" || fields["question"] != "what?" || fields["session_id"] != "w1" || v.Status != RequestRunning {
		t.Fatalf("vision %v %+v", fields, v)
	}
	if _, ok := fields["source"]; ok {
		t.Fatal("empty source sent")
	}

	f2, c2 := newFakeCloud(t, 200, `{"id":"m1","kind":"moderation-audio","status":"completed","charged_microcredits":"40",
		"result":{"policy":"text_safety","decision":"pass","evidence":{"model":"x","yes_probability":0.01},"transcript":"hello","usage":{"audio_samples":16000,"sample_rate":16000}}}`, nil)
	m, err := c2.Moderation.Audio(context.Background(), ModerateAudioOptions{Audio: make([]byte, 32000), IdempotencyKey: "m_1", Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	fields, files = readForm(t, f2)
	if f2.path != "/v1/moderation/audio" || len(files["audio"]) != 32000 || fields["language"] != "en" || m.Result.Decision != "pass" || m.Result.Transcript != "hello" {
		t.Fatalf("moderation %v %+v", fields, m)
	}

	_, c3 := newFakeCloud(t, 200, `{"id":"m1","kind":"vision","status":"completed","charged_microcredits":"1"}`, nil)
	if _, err := c3.Moderation.Get(context.Background(), "m1"); err == nil {
		t.Fatal("non-moderation response accepted")
	}
}

func TestModerationStreamTicket(t *testing.T) {
	_, c := newFakeCloud(t, 200, `{"token":"tk","websocket_path":"/v1/moderation/stream","expires_at":"2026-11-17T10:10:00Z","protocol":"dtelecom.moderation.v1","max_inflight":1,"audio_format":"pcm_s16le","sample_rate":16000,"channels":1,"max_audio_seconds":30}`, nil)
	ticket, err := c.Moderation.StreamTicket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ticket.WebSocketURL, "ws://127.0.0.1:") || !strings.HasSuffix(ticket.WebSocketURL, "/v1/moderation/stream") || ticket.MaxInflight != 1 {
		t.Fatalf("ticket %+v", ticket)
	}
	c.http.baseURL = "https://cloud.example"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"token":"tk","websocket_path":"/v1/moderation/stream"}`)
	}))
	defer srv.Close()
	c.http.client = &http.Client{Transport: rewriteTransport{srv.URL}}
	ticket, err = c.Moderation.StreamTicket(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ticket.WebSocketURL != "wss://cloud.example/v1/moderation/stream" {
		t.Fatalf("https ticket %q", ticket.WebSocketURL)
	}
}

// rewriteTransport sends every request to target, keeping the path.
type rewriteTransport struct{ target string }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	t, _ := http.NewRequest(r.Method, rt.target+u.Path, r.Body)
	t.Header = r.Header
	return http.DefaultTransport.RoundTrip(t.WithContext(r.Context()))
}
