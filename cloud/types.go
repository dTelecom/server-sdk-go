package cloud

import (
	"io"
	"time"
)

// ---- Speech sessions (live STT and TTS on a speech server) ----

// STTSessionOptions open a live speech-to-text session.
type STTSessionOptions struct {
	// Duration is prepaid streaming time, rounded up to whole minutes, at most 24 hours.
	// Unused time is released when the session ends.
	Duration time.Duration
	// Language code, such as en.
	Language string
	// WebRTCSessionID links the session to a room participant (SessionID from CreateToken).
	WebRTCSessionID string
}

// TTSSessionOptions open a live text-to-speech session.
type TTSSessionOptions struct {
	// MaxCharacters the session may synthesise. Extend with TTS.Extend.
	MaxCharacters int
	// Language code, such as en.
	Language string
	// Voice is recorded for the dashboard; the voice you hear is set in the stream's first
	// message.
	Voice string
	// WebRTCSessionID links the session to a room participant (SessionID from CreateToken).
	WebRTCSessionID string
}

// SpeechSession is a prepaid session on a speech server. Its key serves one WebSocket
// connection: after any disconnect, open a new session.
type SpeechSession struct {
	SessionID string
	// Token is the session key for the speech server (session_key in its protocol).
	Token string
	// ServerURL is the speech server origin, as signed into the key.
	ServerURL string
	// StreamURL is the WebSocket address to open: ServerURL + /v1/stream.
	StreamURL string
	ExpiresAt time.Time
}

// ExtendedSTTSession carries the new key; send it to the open socket as
// {"type": "extend", "session_key": token}.
type ExtendedSTTSession struct {
	Token     string
	ExpiresAt time.Time
}

// ExtendedTTSSession carries the new key; send it to the open socket as
// {"type": "extend", "session_key": token}.
type ExtendedTTSSession struct {
	Token string
	// MaxCharacters the session may now synthesise in total.
	MaxCharacters int
	ExpiresAt     time.Time
}

// ---- File jobs (transcription and WAV synthesis) ----

// JobStatus is the state of a file job: uploading, queued, running, completed, failed,
// cancelled or deleted.
type JobStatus string

const (
	JobUploading JobStatus = "uploading"
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
	JobDeleted   JobStatus = "deleted"
)

// TranscriptionOptions start a file transcription.
type TranscriptionOptions struct {
	// File is audio or video, up to 2 hours and 256 MiB. It is streamed, not buffered.
	File io.Reader
	// Filename is sent with the upload; audio.media when empty.
	Filename string
	// IdempotencyKey is 1-128 printable ASCII characters and is bound to this file: reuse it to
	// retry the same upload; with another file it fails with idempotency_conflict (409).
	IdempotencyKey string
	// Language code, en when empty. auto is not accepted for files.
	Language string
	// Diarize labels speakers; on when nil.
	Diarize *bool
}

// TranscriptionSegment is a timed piece of the transcript.
type TranscriptionSegment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker *string `json:"speaker"`
	Text    string  `json:"text"`
}

// SpeakerSegment is a span spoken by one speaker.
type SpeakerSegment struct {
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Speaker string  `json:"speaker"`
}

// TranscriptionMedia describes the decoded media.
type TranscriptionMedia struct {
	SampleCount     int64   `json:"sample_count"`
	SampleRate      int     `json:"sample_rate"`
	Channels        int     `json:"channels"`
	DurationSeconds float64 `json:"duration_seconds"`
}

// TranscriptionResult is a finished transcript.
type TranscriptionResult struct {
	Text     string                 `json:"text"`
	Language string                 `json:"language"`
	Segments []TranscriptionSegment `json:"segments"`
	Media    TranscriptionMedia     `json:"media"`
	// SpeakerSegments label who speaks when (speaker_1, speaker_2, ...), with diarization.
	SpeakerSegments []SpeakerSegment `json:"speaker_segments,omitempty"`
	// SpeakerLabels explains the labels: anonymous and file-local; overlapping voices are not
	// separated.
	SpeakerLabels string `json:"speaker_labels,omitempty"`
	TimingType    string `json:"timing_type"`
}

// TranscriptionJob is a file transcription.
type TranscriptionJob struct {
	ID     string    `json:"id"`
	Status JobStatus `json:"status"`
	// DurationSeconds is the media length, once decoded.
	DurationSeconds *float64             `json:"duration_seconds,omitempty"`
	Result          *TranscriptionResult `json:"result,omitempty"`
	// Error is the failure code when Status is failed.
	Error string `json:"error,omitempty"`
}

// WAVOptions start a WAV synthesis.
type WAVOptions struct {
	// Text or SSML, up to 10,000 characters.
	Text string
	// IdempotencyKey is 1-128 printable ASCII characters. Reusing it with different text fails
	// with idempotency_conflict (409).
	IdempotencyKey string
	Voice          string
	Language       string
	// Speed when non-zero.
	Speed float64
	// InputFormat is text or ssml; text when empty.
	InputFormat string
	// Dictionary maps a locale to source text and its spoken alias. SSML only.
	Dictionary map[string]map[string]string
	// Watermark adds an audio watermark; off when nil.
	Watermark *bool
}

// WAVResult describes a finished 48 kHz mono PCM16 WAV file.
type WAVResult struct {
	Characters int    `json:"characters"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Encoding   string `json:"encoding"`
	Samples    int64  `json:"samples"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Watermark  bool   `json:"watermark"`
}

// WAVJob is a WAV synthesis.
type WAVJob struct {
	ID         string     `json:"id"`
	Status     JobStatus  `json:"status"`
	Characters *int       `json:"characters,omitempty"`
	Result     *WAVResult `json:"result,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// ---- Requests (translation, vision, moderation) ----

// RequestStatus is running, completed or failed. A failed request is returned by the SDK as an
// *APIError, so a returned request is running or completed.
type RequestStatus string

const (
	RequestRunning   RequestStatus = "running"
	RequestCompleted RequestStatus = "completed"
	RequestFailed    RequestStatus = "failed"
)

// Request is a translation, vision or moderation request. Running means call again with the
// same idempotency key, or poll with the service's Get.
type Request[R any] struct {
	ID     string        `json:"id"`
	Kind   string        `json:"kind"`
	Status RequestStatus `json:"status"`
	// ChargedMicrocredits is the charge as a decimal string.
	ChargedMicrocredits string `json:"charged_microcredits"`
	Result              *R     `json:"result,omitempty"`
	Error               string `json:"error,omitempty"`
}

// TranslateOptions describe a translation.
type TranslateOptions struct {
	// Q is the text, billed per Unicode code point, up to 500.
	Q string
	// Target language code.
	Target string
	// Source language code; detected when empty.
	Source string
	// IdempotencyKey is 1-128 letters, digits, underscores or hyphens.
	IdempotencyKey string
}

// TranslationResult is a finished translation.
type TranslationResult struct {
	TranslatedText string `json:"translated_text"`
	// Source is the requested source, auto when it was detected.
	Source           string            `json:"source"`
	Target           string            `json:"target"`
	DetectedLanguage *DetectedLanguage `json:"detected_language,omitempty"`
	Usage            struct {
		// Characters are billed; OutputTokens is informational.
		Characters   int `json:"characters"`
		OutputTokens int `json:"output_tokens,omitempty"`
	} `json:"usage"`
}

// DetectedLanguage is the language the model detected.
type DetectedLanguage struct {
	Language   string  `json:"language"`
	Confidence float64 `json:"confidence"`
}

// Translation is a translation request.
type Translation = Request[TranslationResult]

// VisionOptions ask a question about an image.
type VisionOptions struct {
	// Image up to 6 MiB.
	Image []byte
	// IdempotencyKey is 1-128 letters, digits, underscores or hyphens.
	IdempotencyKey string
	Question       string
	// Source is camera or screen; camera when empty.
	Source string
	// WebRTCSessionID links the request to a room participant.
	WebRTCSessionID string
}

// VisionResult is an answer about an image.
type VisionResult struct {
	RequestID               string `json:"request_id"`
	Answer                  string `json:"answer"`
	Model                   string `json:"model"`
	GeneratedTokens         int    `json:"generated_tokens"`
	PromptTokens            int    `json:"prompt_tokens"`
	FinishReason            string `json:"finish_reason"`
	OutputTokenLimitReached bool   `json:"output_token_limit_reached"`
	Usage                   struct {
		CompletedSnapshots int `json:"completed_snapshots"`
	} `json:"usage"`
}

// VisionSnapshot is a vision request.
type VisionSnapshot = Request[VisionResult]

// ModerateAudioOptions moderate an audio chunk.
type ModerateAudioOptions struct {
	// Audio is mono PCM16 little-endian at 16 kHz, up to 30 seconds.
	Audio []byte
	// IdempotencyKey is 1-128 letters, digits, underscores or hyphens.
	IdempotencyKey string
	Language       string
	// WebRTCSessionID links the request to a room participant.
	WebRTCSessionID string
}

// ModerateVideoFrameOptions moderate one video frame.
type ModerateVideoFrameOptions struct {
	// Image up to 6 MiB.
	Image []byte
	// IdempotencyKey is 1-128 letters, digits, underscores or hyphens.
	IdempotencyKey string
	// WebRTCSessionID links the request to a room participant.
	WebRTCSessionID string
}

// ModerationResult is a moderation decision.
type ModerationResult struct {
	// Policy is text_safety (audio) or visual_safety (video).
	Policy        string `json:"policy"`
	PolicyVersion string `json:"policy_version"`
	// EvaluatedCategories are the categories the combined policy covers, not one prediction
	// per category.
	EvaluatedCategories []string `json:"evaluated_categories"`
	// Decision is pass or flag.
	Decision string `json:"decision"`
	Evidence struct {
		Model          string  `json:"model"`
		YesProbability float64 `json:"yes_probability"`
		Calibrated     bool    `json:"calibrated"`
		ScoreType      string  `json:"score_type"`
	} `json:"evidence"`
	ImageScreening *struct {
		Model        string             `json:"model"`
		NativeScores map[string]float64 `json:"native_scores"`
		Calibrated   bool               `json:"calibrated"`
	} `json:"image_screening,omitempty"`
	// Transcript and Language are set for audio.
	Transcript string `json:"transcript,omitempty"`
	Language   string `json:"language,omitempty"`
	Usage      struct {
		CompletedFrames int   `json:"completed_frames,omitempty"`
		AudioSamples    int64 `json:"audio_samples,omitempty"`
		SampleRate      int   `json:"sample_rate,omitempty"`
	} `json:"usage"`
}

// Moderation is a moderation request; Kind is moderation-audio or moderation-video.
type Moderation = Request[ModerationResult]

// ModerationStreamTicket opens one moderation WebSocket. It holds no API secret, so a server
// can hand it to a browser.
type ModerationStreamTicket struct {
	Token string
	// WebSocketURL is the wss:// URL of the stream.
	WebSocketURL    string
	ExpiresAt       time.Time
	Protocol        string
	MaxInflight     int
	AudioFormat     string
	SampleRate      int
	Channels        int
	MaxAudioSeconds int
}
