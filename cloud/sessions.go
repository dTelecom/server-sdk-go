package cloud

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SessionCharge is a session of the project and what it was charged.
type SessionCharge struct {
	SessionID string `json:"session_id"`
	// Service is "webrtc" for a participant, else the service: "stt", "tts", "file-stt", "vision", ...
	Service string `json:"service"`
	// Status is pending, active, completed, expired, failed or rejected.
	Status string `json:"status"`
	// RoomName and Identity are the participant's; for a service session, those of its linked participant.
	RoomName string `json:"room_name,omitempty"`
	Identity string `json:"identity,omitempty"`
	// WebRTCSessionID is the participant session a service session was linked to.
	WebRTCSessionID string `json:"webrtc_session_id,omitempty"`
	// Region is the node location of a participant session.
	Region    string     `json:"region,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// Characters are the TTS characters used.
	Characters int64 `json:"characters,omitempty"`
	// ChargedMicrocredits is the charge (1,000,000 per dollar) as a decimal string; ChargedUSD the same in dollars.
	ChargedMicrocredits string `json:"charged_microcredits"`
	ChargedUSD          string `json:"charged_usd"`
	// Final is true once the session ended or was rejected; until then the charge is 0.
	Final bool `json:"final"`
	// CoveredByAddon is true when the bandwidth add-on covered this participant session.
	CoveredByAddon bool `json:"covered_by_addon"`
}

// SessionCharges is a room's sessions, newest first, and their total.
type SessionCharges struct {
	Sessions          []SessionCharge `json:"sessions"`
	TotalMicrocredits string          `json:"total_microcredits"`
	TotalUSD          string          `json:"total_usd"`
}

// ListSessionsOptions narrows ListSessions.
type ListSessionsOptions struct {
	// Since drops sessions created before it; a room name can be reused.
	Since time.Time
	// Limit caps the result: default 100, at most 500.
	Limit int
}

// GetSession reads a session of the project (a participant from CreateToken, or a service
// session) and what it was charged. Sessions are charged when they end: check Final.
func (c *Client) GetSession(ctx context.Context, sessionID string) (*SessionCharge, error) {
	var res SessionCharge
	if err := c.http.json(ctx, http.MethodGet, "/v1/sessions/"+url.PathEscape(sessionID), nil, nil, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ListSessions lists a room's participant sessions and the service sessions linked to them,
// newest first, with their charges and the total.
func (c *Client) ListSessions(ctx context.Context, room string, opts ListSessionsOptions) (*SessionCharges, error) {
	query := url.Values{"room": {room}}
	if !opts.Since.IsZero() {
		query.Set("since", opts.Since.UTC().Format(time.RFC3339Nano))
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	var res SessionCharges
	if err := c.http.json(ctx, http.MethodGet, "/v1/sessions?"+query.Encode(), nil, nil, &res); err != nil {
		return nil, err
	}
	if res.Sessions == nil {
		res.Sessions = []SessionCharge{}
	}
	return &res, nil
}
