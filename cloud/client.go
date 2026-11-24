// Package cloud is the Go client of dTelecom Cloud: participant tokens and room administration,
// live speech sessions, file transcription, WAV synthesis, translation, vision, moderation and
// node webhooks.
//
// Use it only on a server: the client holds the project's API secret.
//
//	c, err := cloud.New(cloud.Options{}) // DTELECOM_API_KEY, DTELECOM_API_SECRET, DTELECOM_CLOUD_URL
//	t, err := c.CreateToken(ctx, cloud.TokenOptions{Room: "demo", Identity: "alice"})
//	// give t.Token and t.WsURL to the client SDK
package cloud

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultTokenTTL = 6 * time.Hour

// Options configure a Client. Empty fields fall back to environment variables.
type Options struct {
	// APIKey of the project. Env: DTELECOM_API_KEY.
	APIKey string
	// APISecret of the key. Env: DTELECOM_API_SECRET.
	APISecret string
	// BaseURL is the cloud origin, such as https://cloud.dtelecom.org. Env: DTELECOM_CLOUD_URL.
	BaseURL string
	// HTTPClient sends the requests; http.DefaultClient when nil.
	HTTPClient *http.Client
}

// Client calls the dTelecom Cloud hosted API. It is safe for concurrent use.
type Client struct {
	http *cloudHTTP

	// STT opens live speech-to-text sessions.
	STT *STTService
	// TTS opens live text-to-speech sessions.
	TTS *TTSService
	// Transcriptions transcribes audio and video files.
	Transcriptions *TranscriptionsService
	// WAV synthesises text or SSML into WAV files.
	WAV *WAVService
	// Vision answers questions about images.
	Vision *VisionService
	// Moderation checks audio and video frames, per item or over a WebSocket.
	Moderation *ModerationService
}

// New returns a client. It fails when the key, secret or base URL is missing.
func New(opts Options) (*Client, error) {
	key := firstNonEmpty(opts.APIKey, os.Getenv("DTELECOM_API_KEY"))
	secret := firstNonEmpty(opts.APISecret, os.Getenv("DTELECOM_API_SECRET"))
	base := firstNonEmpty(opts.BaseURL, os.Getenv("DTELECOM_CLOUD_URL"))
	if key == "" || secret == "" {
		return nil, errors.New("dtelecom cloud: APIKey and APISecret are required (or DTELECOM_API_KEY, DTELECOM_API_SECRET)")
	}
	if base == "" {
		return nil, errors.New("dtelecom cloud: BaseURL is required (or DTELECOM_CLOUD_URL)")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	h := &cloudHTTP{
		authorization: "Bearer " + key + ":" + secret,
		baseURL:       strings.TrimRight(base, "/"),
		client:        hc,
	}
	return &Client{
		http:           h,
		STT:            &STTService{http: h},
		TTS:            &TTSService{http: h},
		Transcriptions: &TranscriptionsService{http: h},
		WAV:            &WAVService{http: h},
		Vision:         &VisionService{http: h},
		Moderation:     &ModerationService{http: h},
	}, nil
}

// Grants narrow a participant's node permissions. Nil fields keep their defaults: publish,
// subscribe and data are allowed, room admin is not.
type Grants struct {
	CanPublish     *bool
	CanSubscribe   *bool
	CanPublishData *bool
	RoomAdmin      bool
}

// Bool returns a pointer to v, for Grants.
func Bool(v bool) *bool { return &v }

// TokenOptions describe a participant token.
type TokenOptions struct {
	// Room name. A room is created when its first participant joins.
	Room string
	// Identity is unique within the room; a second connection with it replaces the first.
	Identity string
	// Name is the display name.
	Name string
	// Metadata is opaque data other participants can read, up to 16 KiB.
	Metadata string
	// TTL is the token lifetime: 6 hours when zero, at most 24 hours, rounded up to whole
	// minutes. Extend the token before it expires, or the participant is removed.
	TTL time.Duration
	// ClientIP is the participant's public IP, used to pick the nearest node. Without it the
	// node is picked near the caller, that is your server.
	ClientIP string
	// WebhookURLs receive this participant's room events: HTTPS only, up to 5.
	WebhookURLs []string
	// Grants override the default permissions.
	Grants *Grants
}

// ParticipantToken is a token for the client SDK and the node to connect to.
type ParticipantToken struct {
	Token string
	// WsURL is the node URL (wss://...) for the client SDK.
	WsURL string
	// SessionID identifies the session for ExtendToken, GetSession and for linking AI usage to it.
	SessionID string
	// Region is where the chosen node is: its city, else its country; empty when unknown.
	Region    string
	ExpiresAt time.Time
}

// ExtendedToken is a token for the whole remaining time of an extended session.
type ExtendedToken struct {
	Token string
	WsURL string
	// Region is the session's node location, as in CreateToken.
	Region    string
	ExpiresAt time.Time
}

// AdminToken authorises room administration on the nodes for 10 minutes.
type AdminToken struct {
	Token string
	// APIURLs are the HTTPS origins of the nodes hosting the room's joined participants.
	// Empty when nobody has joined.
	APIURLs   []string
	ExpiresAt time.Time
}

// CreateToken creates a participant session and returns its token and node URL. Every issued
// token counts toward the plan's concurrent participants from the moment it is issued.
func (c *Client) CreateToken(ctx context.Context, opts TokenOptions) (*ParticipantToken, error) {
	ttl := opts.TTL
	if ttl == 0 {
		ttl = defaultTokenTTL
	}
	minutes, err := durationMinutes(ttl)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"room_name":        opts.Room,
		"identity":         opts.Identity,
		"duration_minutes": minutes,
	}
	if opts.Name != "" {
		body["name"] = opts.Name
	}
	if opts.Metadata != "" {
		body["metadata"] = opts.Metadata
	}
	if opts.ClientIP != "" {
		body["client_ip"] = opts.ClientIP
	}
	if len(opts.WebhookURLs) > 0 {
		body["webhook_urls"] = opts.WebhookURLs
	}
	if g := opts.Grants; g != nil {
		grants := map[string]bool{}
		if g.CanPublish != nil {
			grants["can_publish"] = *g.CanPublish
		}
		if g.CanSubscribe != nil {
			grants["can_subscribe"] = *g.CanSubscribe
		}
		if g.CanPublishData != nil {
			grants["can_publish_data"] = *g.CanPublishData
		}
		if g.RoomAdmin {
			grants["room_admin"] = true
		}
		body["grants"] = grants
	}
	var res struct {
		SessionID string    `json:"session_id"`
		Token     string    `json:"token"`
		WsURL     string    `json:"ws_url"`
		Region    string    `json:"region"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.http.json(ctx, http.MethodPost, "/v1/webrtc/token", body, nil, &res); err != nil {
		return nil, err
	}
	return &ParticipantToken{Token: res.Token, WsURL: res.WsURL, SessionID: res.SessionID, Region: res.Region, ExpiresAt: res.ExpiresAt}, nil
}

// ExtendToken adds time to a session and returns a token for the whole remaining time. The
// current connection stays up; the client uses the new token when it connects again. It keeps
// the name, metadata, grants and webhook URLs.
//
// Call it before the token expires: a participant whose token expired without an extension is
// removed about a minute later, and later calls fail with code session_not_extendable (409).
// additional is rounded up to whole minutes, at most 24 hours per call.
func (c *Client) ExtendToken(ctx context.Context, sessionID string, additional time.Duration) (*ExtendedToken, error) {
	minutes, err := durationMinutes(additional)
	if err != nil {
		return nil, err
	}
	var res struct {
		Token        string    `json:"token"`
		WsURL        string    `json:"ws_url"`
		Region       string    `json:"region"`
		NewExpiresAt time.Time `json:"new_expires_at"`
	}
	body := map[string]any{"session_id": sessionID, "additional_minutes": minutes}
	if err := c.http.json(ctx, http.MethodPost, "/v1/webrtc/token/extend", body, nil, &res); err != nil {
		return nil, err
	}
	return &ExtendedToken{Token: res.Token, WsURL: res.WsURL, Region: res.Region, ExpiresAt: res.NewExpiresAt}, nil
}

// AdminToken mints a 10-minute room admin token and lists the nodes hosting the room.
// RoomService uses it; call it yourself only to reach the nodes' API directly.
func (c *Client) AdminToken(ctx context.Context, room string) (*AdminToken, error) {
	var res struct {
		Token     string    `json:"token"`
		APIURLs   []string  `json:"api_urls"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.http.json(ctx, http.MethodPost, "/v1/webrtc/admin-token", map[string]any{"room_name": room}, nil, &res); err != nil {
		return nil, err
	}
	if res.APIURLs == nil {
		res.APIURLs = []string{}
	}
	return &AdminToken{Token: res.Token, APIURLs: res.APIURLs, ExpiresAt: res.ExpiresAt}, nil
}

// RoomService administers rooms on the nodes, authorised through AdminToken. Keep the returned
// value to reuse its cached admin tokens.
func (c *Client) RoomService() *RoomService {
	return &RoomService{cloud: c, admin: map[string]*AdminToken{}, client: c.http.client}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
