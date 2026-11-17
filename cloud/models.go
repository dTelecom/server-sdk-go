package cloud

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// Room is a room as the nodes describe it.
type Room struct {
	SID             string `json:"sid"`
	Name            string `json:"name"`
	EmptyTimeout    uint32 `json:"emptyTimeout"`
	MaxParticipants uint32 `json:"maxParticipants"`
	// CreationTime is a Unix time in seconds.
	CreationTime    Int64  `json:"creationTime"`
	Metadata        string `json:"metadata"`
	NumParticipants uint32 `json:"numParticipants"`
	ActiveRecording bool   `json:"activeRecording"`
	// TurnPassword is a single-use TURN password for the participant the event is about.
	TurnPassword string `json:"turnPassword,omitempty"`
	// Key is the project's room key, on participant and track events from cloud tokens.
	Key string `json:"key,omitempty"`
}

// ParticipantPermission are a participant's node permissions.
type ParticipantPermission struct {
	CanSubscribe      bool     `json:"canSubscribe"`
	CanPublish        bool     `json:"canPublish"`
	CanPublishData    bool     `json:"canPublishData"`
	CanPublishSources []string `json:"canPublishSources,omitempty"`
	Hidden            bool     `json:"hidden"`
	Recorder          bool     `json:"recorder"`
	CanUpdateMetadata bool     `json:"canUpdateMetadata"`
}

// ParticipantInfo describes a participant.
type ParticipantInfo struct {
	SID      string `json:"sid"`
	Identity string `json:"identity"`
	// State is JOINING, JOINED, ACTIVE or DISCONNECTED.
	State    Enum        `json:"state"`
	Tracks   []TrackInfo `json:"tracks,omitempty"`
	Metadata string      `json:"metadata"`
	// JoinedAt is a Unix time in seconds.
	JoinedAt    Int64                  `json:"joinedAt"`
	Name        string                 `json:"name"`
	Version     uint32                 `json:"version"`
	Permission  *ParticipantPermission `json:"permission,omitempty"`
	Region      string                 `json:"region"`
	IsPublisher bool                   `json:"isPublisher"`
	Relayed     bool                   `json:"relayed"`
	// RTTMs is the round trip time to the node, set on participant_left; 0 when not measured.
	RTTMs uint32 `json:"rttMs"`
}

// VideoLayer is one simulcast layer of a video track.
type VideoLayer struct {
	// Quality is LOW, MEDIUM, HIGH or OFF.
	Quality Enum   `json:"quality"`
	Width   uint32 `json:"width"`
	Height  uint32 `json:"height"`
	Bitrate uint32 `json:"bitrate"`
	SSRC    uint32 `json:"ssrc"`
}

// TrackInfo describes a published track.
type TrackInfo struct {
	SID string `json:"sid"`
	// Type is AUDIO, VIDEO or DATA.
	Type      Enum   `json:"type"`
	Name      string `json:"name"`
	Muted     bool   `json:"muted"`
	Width     uint32 `json:"width"`
	Height    uint32 `json:"height"`
	Simulcast bool   `json:"simulcast"`
	// Source is CAMERA, MICROPHONE, SCREEN_SHARE, SCREEN_SHARE_AUDIO or UNKNOWN.
	Source   Enum         `json:"source"`
	Layers   []VideoLayer `json:"layers,omitempty"`
	MimeType string       `json:"mimeType"`
	Mid      string       `json:"mid"`
	Stereo   bool         `json:"stereo"`
}

// Int64 is an int64 that decodes from a JSON number or a string, as protobuf JSON writes int64.
type Int64 int64

func (v *Int64) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*v = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*v = Int64(n)
	return nil
}

// Enum is a protobuf enum value by name, such as AUDIO. Numbers sent by older nodes are kept as
// their decimal text.
type Enum string

func (e *Enum) UnmarshalJSON(data []byte) error {
	if bytes.HasPrefix(data, []byte(`"`)) {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*e = Enum(s)
		return nil
	}
	*e = Enum(string(data))
	return nil
}

// camelizeJSON rewrites snake_case object keys to camelCase, recursively. The node API answers
// with proto field names; webhooks already use camelCase.
func camelizeJSON(data []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(camelizeValue(v))
}

func camelizeValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[camelKey(k)] = camelizeValue(item)
		}
		return out
	case []any:
		for i := range t {
			t[i] = camelizeValue(t[i])
		}
		return t
	default:
		return v
	}
}

func camelKey(k string) string {
	if !strings.Contains(k, "_") {
		return k
	}
	parts := strings.Split(k, "_")
	var b strings.Builder
	b.WriteString(parts[0])
	for _, p := range parts[1:] {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}
