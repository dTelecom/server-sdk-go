package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const adminTokenRefreshMargin = time.Minute

// ErrRoomNotLive means the room has no joined participant, so there is no node to call.
var ErrRoomNotLive = errors.New("dtelecom cloud: room has no joined participants")

// NodeAPIError is a node refusing or failing a room API call.
type NodeAPIError struct {
	// APIURL is the node that answered.
	APIURL string
	// Status is the HTTP status; 0 when the node was not reached.
	Status int
	// Code is the Twirp error code, such as not_found.
	Code    string
	Message string
}

func (e *NodeAPIError) Error() string {
	return fmt.Sprintf("%s: %s", e.APIURL, e.Message)
}

// definitive reports whether the node answered with a client error, which another node would
// repeat.
func (e *NodeAPIError) definitive() bool {
	return e.Status >= 400 && e.Status < 500
}

// DataKind is how a data message is delivered.
type DataKind int

const (
	// DataReliable is delivered in order and retransmitted when lost.
	DataReliable DataKind = 0
	// DataLossy may be dropped.
	DataLossy DataKind = 1
)

// SendDataOptions narrow a data message.
type SendDataOptions struct {
	// DestinationSIDs are the participants to receive it; everyone when empty.
	DestinationSIDs []string
	Topic           string
}

// UpdateParticipantOptions change a participant. Empty fields are left as they are.
type UpdateParticipantOptions struct {
	Metadata   string
	Name       string
	Permission *ParticipantPermission
}

// RoomService administers rooms through the nodes' Twirp API, authorised with admin tokens from
// the cloud. The admin token lists the nodes hosting the room's participants. A node answers
// reads from its own participants only, and applies a change to a participant only when it hosts
// that participant, so calls about a participant go to its node, room-wide calls to one hosting
// node, and ListParticipants to all of them. It is safe for concurrent use.
type RoomService struct {
	cloud  *Client
	client *http.Client
	mu     sync.Mutex
	admin  map[string]*AdminToken
}

// DeleteRoom ends the room. A room nobody joined is already gone.
func (s *RoomService) DeleteRoom(ctx context.Context, room string) error {
	nodes, err := s.nodes(ctx, room)
	if err != nil || len(nodes.APIURLs) == 0 {
		return err
	}
	return s.onAnyNode(ctx, nodes, "DeleteRoom", map[string]any{"room": room}, nil)
}

// ListParticipants lists the joined participants across the room's nodes.
func (s *RoomService) ListParticipants(ctx context.Context, room string) ([]ParticipantInfo, error) {
	nodes, err := s.nodes(ctx, room)
	if err != nil {
		return nil, err
	}
	out := []ParticipantInfo{}
	if len(nodes.APIURLs) == 0 {
		return out, nil
	}
	lists, err := s.participantsByNode(ctx, nodes, room)
	if err != nil {
		return nil, err
	}
	for _, l := range lists {
		out = append(out, l.participants...)
	}
	return out, nil
}

// UpdateRoomMetadata sets the room's metadata and returns the room. Participants get the
// metadata through their room metadata callback. Nodes released before 2026-11-17 answer with an
// empty Room, although the metadata is set.
func (s *RoomService) UpdateRoomMetadata(ctx context.Context, room, metadata string) (*Room, error) {
	nodes, err := s.liveNodes(ctx, room)
	if err != nil {
		return nil, err
	}
	var r Room
	if err := s.onAnyNode(ctx, nodes, "UpdateRoomMetadata", map[string]any{"room": room, "metadata": metadata}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetParticipant reads one participant.
func (s *RoomService) GetParticipant(ctx context.Context, room, identity string) (*ParticipantInfo, error) {
	var p ParticipantInfo
	if err := s.onParticipantNode(ctx, room, identity, "GetParticipant", map[string]any{"room": room, "identity": identity}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// RemoveParticipant disconnects a participant. They can join again with a valid token.
func (s *RoomService) RemoveParticipant(ctx context.Context, room, identity string) error {
	return s.onParticipantNode(ctx, room, identity, "RemoveParticipant", map[string]any{"room": room, "identity": identity}, nil)
}

// MutePublishedTrack mutes or unmutes a participant's track and returns the track.
func (s *RoomService) MutePublishedTrack(ctx context.Context, room, identity, trackSID string, muted bool) (*TrackInfo, error) {
	var res struct {
		Track *TrackInfo `json:"track"`
	}
	err := s.onParticipantNode(ctx, room, identity, "MutePublishedTrack", map[string]any{
		"room": room, "identity": identity, "trackSid": trackSID, "muted": muted,
	}, &res)
	if err != nil {
		return nil, err
	}
	return res.Track, nil
}

// UpdateParticipant changes a participant's metadata, name or permissions and returns the
// participant. Nodes released before 2026-11-17 answer with an empty ParticipantInfo, although
// the change is applied.
func (s *RoomService) UpdateParticipant(ctx context.Context, room, identity string, opts UpdateParticipantOptions) (*ParticipantInfo, error) {
	body := map[string]any{"room": room, "identity": identity, "metadata": opts.Metadata, "name": opts.Name}
	if opts.Permission != nil {
		body["permission"] = opts.Permission
	}
	var p ParticipantInfo
	if err := s.onParticipantNode(ctx, room, identity, "UpdateParticipant", body, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// UpdateSubscriptions subscribes a participant to tracks, or unsubscribes it. Tracks are sent
// grouped by their publisher, which every node release accepts.
func (s *RoomService) UpdateSubscriptions(ctx context.Context, room, identity string, trackSIDs []string, subscribe bool) error {
	nodes, err := s.liveNodes(ctx, room)
	if err != nil {
		return err
	}
	lists, err := s.participantsByNode(ctx, nodes, room)
	if err != nil {
		return err
	}
	owner := map[string]string{}
	subscriberNode := nodes.APIURLs[0]
	for _, l := range lists {
		for _, p := range l.participants {
			if p.Identity == identity {
				subscriberNode = l.apiURL
			}
			for _, t := range p.Tracks {
				owner[t.SID] = p.SID
			}
		}
	}
	grouped := map[string][]string{}
	var order []string
	for _, sid := range trackSIDs {
		pSID, ok := owner[sid]
		if !ok {
			return fmt.Errorf("dtelecom cloud: track %s is not published in room %s", sid, room)
		}
		if _, seen := grouped[pSID]; !seen {
			order = append(order, pSID)
		}
		grouped[pSID] = append(grouped[pSID], sid)
	}
	tracks := []map[string]any{}
	for _, pSID := range order {
		tracks = append(tracks, map[string]any{"participantSid": pSID, "trackSids": grouped[pSID]})
	}
	return s.call(ctx, subscriberNode, nodes.Token, "UpdateSubscriptions", map[string]any{
		"room": room, "identity": identity, "subscribe": subscribe, "participantTracks": tracks,
	}, nil)
}

// SendData sends a data message to the room, or to opts.DestinationSIDs only. The node that
// takes it delivers it to its participants and relays it to the room's other nodes, so it goes
// to one hosting node.
func (s *RoomService) SendData(ctx context.Context, room string, data []byte, kind DataKind, opts SendDataOptions) error {
	nodes, err := s.liveNodes(ctx, room)
	if err != nil {
		return err
	}
	kindName := "RELIABLE"
	if kind == DataLossy {
		kindName = "LOSSY"
	}
	dest := opts.DestinationSIDs
	if dest == nil {
		dest = []string{}
	}
	body := map[string]any{"room": room, "data": data, "kind": kindName, "destinationSids": dest}
	if opts.Topic != "" {
		body["topic"] = opts.Topic
	}
	return s.onAnyNode(ctx, nodes, "SendData", body, nil)
}

// nodes returns a cached admin token for the room, or a fresh one.
func (s *RoomService) nodes(ctx context.Context, room string) (*AdminToken, error) {
	s.mu.Lock()
	cached := s.admin[room]
	s.mu.Unlock()
	if cached != nil && time.Until(cached.ExpiresAt) > adminTokenRefreshMargin {
		return cached, nil
	}
	fresh, err := s.cloud.AdminToken(ctx, room)
	if err != nil {
		return nil, err
	}
	// A room nobody has joined yet has no nodes; ask again next time instead of caching that.
	if len(fresh.APIURLs) > 0 {
		s.mu.Lock()
		s.admin[room] = fresh
		s.mu.Unlock()
	}
	return fresh, nil
}

// liveNodes is nodes for calls that need a joined participant.
func (s *RoomService) liveNodes(ctx context.Context, room string) (*AdminToken, error) {
	nodes, err := s.nodes(ctx, room)
	if err != nil {
		return nil, err
	}
	if len(nodes.APIURLs) == 0 {
		return nil, ErrRoomNotLive
	}
	return nodes, nil
}

type nodeParticipants struct {
	apiURL       string
	participants []ParticipantInfo
}

// participantsByNode asks every hosting node for its participants at once. It returns the
// answers of the nodes that replied, or the first error when none did.
func (s *RoomService) participantsByNode(ctx context.Context, nodes *AdminToken, room string) ([]nodeParticipants, error) {
	type answer struct {
		list nodeParticipants
		err  error
	}
	answers := make([]answer, len(nodes.APIURLs))
	var wg sync.WaitGroup
	for i, apiURL := range nodes.APIURLs {
		wg.Add(1)
		go func(i int, apiURL string) {
			defer wg.Done()
			var res struct {
				Participants []ParticipantInfo `json:"participants"`
			}
			err := s.call(ctx, apiURL, nodes.Token, "ListParticipants", map[string]any{"room": room}, &res)
			answers[i] = answer{nodeParticipants{apiURL, res.Participants}, err}
		}(i, apiURL)
	}
	wg.Wait()
	var lists []nodeParticipants
	var firstErr error
	for _, a := range answers {
		if a.err == nil {
			lists = append(lists, a.list)
		} else if firstErr == nil {
			firstErr = a.err
		}
	}
	if len(lists) == 0 {
		return nil, firstErr
	}
	return lists, nil
}

// onParticipantNode runs a call on the node hosting the participant. With several nodes it finds
// that node from their participant lists; when none lists the participant, the first node answers.
func (s *RoomService) onParticipantNode(ctx context.Context, room, identity, method string, body any, out any) error {
	nodes, err := s.liveNodes(ctx, room)
	if err != nil {
		return err
	}
	target := nodes.APIURLs[0]
	if len(nodes.APIURLs) > 1 {
		lists, err := s.participantsByNode(ctx, nodes, room)
		if err != nil {
			return err
		}
	find:
		for _, l := range lists {
			for _, p := range l.participants {
				if p.Identity == identity {
					target = l.apiURL
					break find
				}
			}
		}
	}
	return s.call(ctx, target, nodes.Token, method, body, out)
}

// onAnyNode runs a room-wide call on the first hosting node that answers. A client error is
// final; an unreachable node or a server error moves on to the next node.
func (s *RoomService) onAnyNode(ctx context.Context, nodes *AdminToken, method string, body any, out any) error {
	var firstErr error
	for _, apiURL := range nodes.APIURLs {
		err := s.call(ctx, apiURL, nodes.Token, method, body, out)
		if err == nil {
			return nil
		}
		var nodeErr *NodeAPIError
		if errors.As(err, &nodeErr) && nodeErr.definitive() {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// call sends one Twirp request and decodes the camelCase answer into out.
func (s *RoomService) call(ctx context.Context, apiURL, token, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("dtelecom cloud: encode %s: %w", method, err)
	}
	url := strings.TrimRight(apiURL, "/") + "/twirp/livekit.RoomService/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return &NodeAPIError{APIURL: apiURL, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := s.client.Do(req)
	if err != nil {
		return &NodeAPIError{APIURL: apiURL, Message: err.Error()}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return &NodeAPIError{APIURL: apiURL, Status: res.StatusCode, Message: err.Error()}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		var twirpErr struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(data, &twirpErr)
		msg := twirpErr.Msg
		if msg == "" {
			msg = res.Status
		}
		return &NodeAPIError{APIURL: apiURL, Status: res.StatusCode, Code: twirpErr.Code, Message: msg}
	}
	if out == nil {
		return nil
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	camel, err := camelizeJSON(trimmed)
	if err != nil {
		return &NodeAPIError{APIURL: apiURL, Status: res.StatusCode, Message: "invalid response: " + err.Error()}
	}
	if err := json.Unmarshal(camel, out); err != nil {
		return fmt.Errorf("dtelecom cloud: decode %s: %w", method, err)
	}
	return nil
}
