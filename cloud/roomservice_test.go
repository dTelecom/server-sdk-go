package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRoomCloud runs a cloud that mints admin tokens for the given nodes.
func fakeRoomCloud(t *testing.T, nodeURLs []string, adminCalls *int32) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(adminCalls, 1)
		urls, _ := json.Marshal(nodeURLs)
		_, _ = io.WriteString(w, `{"token":"adm","api_urls":`+string(urls)+`,"expires_at":"`+time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339)+`"}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Options{APIKey: "k", APISecret: "s", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type nodeCall struct {
	method, auth string
	body         map[string]any
}

// stubNode is a node whose answer comes from reply(method, body).
type stubNode struct {
	url   string
	mu    sync.Mutex
	calls []nodeCall
}

func (n *stubNode) methods() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, c := range n.calls {
		out = append(out, c.method)
	}
	return out
}

func (n *stubNode) last(method string) nodeCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := len(n.calls) - 1; i >= 0; i-- {
		if n.calls[i].method == method {
			return n.calls[i]
		}
	}
	return nodeCall{}
}

func newStubNode(t *testing.T, reply func(method string, body map[string]any) (int, string)) *stubNode {
	t.Helper()
	n := &stubNode{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if !strings.HasPrefix(r.URL.Path, "/twirp/livekit.RoomService/") {
			t.Errorf("path %s", r.URL.Path)
		}
		n.mu.Lock()
		n.calls = append(n.calls, nodeCall{method, r.Header.Get("Authorization"), body})
		n.mu.Unlock()
		status, out := reply(method, body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, out)
	}))
	t.Cleanup(srv.Close)
	n.url = srv.URL
	return n
}

// hosting is a node that hosts the given participants and accepts every other call.
func hosting(t *testing.T, participants string) *stubNode {
	return newStubNode(t, func(method string, _ map[string]any) (int, string) {
		if method == "ListParticipants" {
			return 200, `{"participants":[` + participants + `]}`
		}
		return 200, `{}`
	})
}

func TestRoomServiceListParticipantsAcrossNodes(t *testing.T) {
	n1 := hosting(t, `{"sid":"PA_1","identity":"alice","state":"ACTIVE","joined_at":"1763370000","tracks":[{"sid":"TR_1","type":"AUDIO","mime_type":"audio/opus"}],"permission":{"can_publish":true}}`)
	n2 := hosting(t, `{"sid":"PA_2","identity":"bob","state":2}`)
	var adminCalls int32
	rs := fakeRoomCloud(t, []string{n1.url, n2.url}, &adminCalls).RoomService()
	ps, err := rs.ListParticipants(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 {
		t.Fatalf("participants %+v", ps)
	}
	byID := map[string]ParticipantInfo{ps[0].Identity: ps[0], ps[1].Identity: ps[1]}
	a := byID["alice"]
	if a.State != "ACTIVE" || a.JoinedAt != 1763370000 || a.Tracks[0].MimeType != "audio/opus" || a.Tracks[0].Type != "AUDIO" || !a.Permission.CanPublish {
		t.Fatalf("alice %+v", a)
	}
	if byID["bob"].State != "2" {
		t.Fatalf("numeric enum %q", byID["bob"].State)
	}
	if c := n1.last("ListParticipants"); c.auth != "Bearer adm" || c.body["room"] != "demo" {
		t.Fatalf("node call %+v", c)
	}
	// The admin token is cached.
	if _, err := rs.ListParticipants(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&adminCalls) != 1 {
		t.Fatalf("admin token fetched %d times", adminCalls)
	}
}

func TestRoomServiceParticipantCallsGoToTheirNode(t *testing.T) {
	n1 := hosting(t, `{"sid":"PA_1","identity":"alice"}`)
	n2 := hosting(t, `{"sid":"PA_2","identity":"bob","tracks":[{"sid":"TR_B"}]}`)
	var adminCalls int32
	rs := fakeRoomCloud(t, []string{n1.url, n2.url}, &adminCalls).RoomService()
	ctx := context.Background()

	if err := rs.RemoveParticipant(ctx, "demo", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.MutePublishedTrack(ctx, "demo", "bob", "TR_B", true); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.UpdateParticipant(ctx, "demo", "alice", UpdateParticipantOptions{Metadata: "m"}); err != nil {
		t.Fatal(err)
	}
	for _, m := range n1.methods() {
		if m == "RemoveParticipant" || m == "MutePublishedTrack" {
			t.Fatalf("bob's call went to alice's node: %v", n1.methods())
		}
	}
	if n2.last("RemoveParticipant").body["identity"] != "bob" || n2.last("MutePublishedTrack").body["trackSid"] != "TR_B" {
		t.Fatalf("bob's node calls %v", n2.methods())
	}
	if n1.last("UpdateParticipant").body["metadata"] != "m" {
		t.Fatalf("alice's node calls %v", n1.methods())
	}
	for _, m := range n2.methods() {
		if m == "UpdateParticipant" {
			t.Fatal("alice's update went to bob's node")
		}
	}

	// Subscriptions: tracks grouped by publisher, sent to the subscriber's node.
	if err := rs.UpdateSubscriptions(ctx, "demo", "alice", []string{"TR_B"}, false); err != nil {
		t.Fatal(err)
	}
	c := n1.last("UpdateSubscriptions")
	got, _ := json.Marshal(c.body["participantTracks"])
	if string(got) != `[{"participantSid":"PA_2","trackSids":["TR_B"]}]` || c.body["subscribe"] != false {
		t.Fatalf("subscriptions %v", c.body)
	}
	if err := rs.UpdateSubscriptions(ctx, "demo", "alice", []string{"TR_X"}, true); err == nil {
		t.Fatal("unknown track accepted")
	}
}

func TestRoomServiceRoomCallsGoToOneNode(t *testing.T) {
	n1 := hosting(t, ``)
	n2 := hosting(t, ``)
	var adminCalls int32
	rs := fakeRoomCloud(t, []string{n1.url, n2.url}, &adminCalls).RoomService()
	ctx := context.Background()
	if err := rs.SendData(ctx, "demo", []byte("hi"), DataLossy, SendDataOptions{Topic: "chat"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.UpdateRoomMetadata(ctx, "demo", "meta"); err != nil {
		t.Fatal(err)
	}
	if err := rs.DeleteRoom(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(n1.methods(), ","); got != "SendData,UpdateRoomMetadata,DeleteRoom" {
		t.Fatalf("first node got %s", got)
	}
	if len(n2.methods()) != 0 {
		t.Fatalf("second node got %v", n2.methods())
	}
	c := n1.last("SendData")
	if c.body["data"] != "aGk=" || c.body["kind"] != "LOSSY" || c.body["topic"] != "chat" {
		t.Fatalf("send data body %v", c.body)
	}
}

func TestRoomServiceFailover(t *testing.T) {
	down := newStubNode(t, func(string, map[string]any) (int, string) { return 503, `{"code":"unavailable","msg":"busy"}` })
	up := hosting(t, ``)
	var adminCalls int32
	rs := fakeRoomCloud(t, []string{down.url, up.url}, &adminCalls).RoomService()
	if err := rs.SendData(context.Background(), "demo", []byte("x"), DataReliable, SendDataOptions{}); err != nil {
		t.Fatalf("second node should take it: %v", err)
	}
	if len(up.methods()) != 1 {
		t.Fatalf("up node calls %v", up.methods())
	}

	refusing := newStubNode(t, func(string, map[string]any) (int, string) { return 400, `{"code":"invalid_argument","msg":"bad"}` })
	other := hosting(t, ``)
	rs = fakeRoomCloud(t, []string{refusing.url, other.url}, &adminCalls).RoomService()
	err := rs.SendData(context.Background(), "demo", []byte("x"), DataReliable, SendDataOptions{})
	var nodeErr *NodeAPIError
	if !errors.As(err, &nodeErr) || nodeErr.Status != 400 || nodeErr.Code != "invalid_argument" || nodeErr.APIURL != refusing.url {
		t.Fatalf("want the node's 400, got %#v", err)
	}
	if len(other.methods()) != 0 {
		t.Fatal("a client error moved on to another node")
	}
}

func TestRoomServiceNodeError(t *testing.T) {
	bad := newStubNode(t, func(string, map[string]any) (int, string) {
		return 404, `{"code":"not_found","msg":"participant does not exist"}`
	})
	var adminCalls int32
	rs := fakeRoomCloud(t, []string{bad.url}, &adminCalls).RoomService()
	_, err := rs.GetParticipant(context.Background(), "demo", "ghost")
	var nodeErr *NodeAPIError
	if !errors.As(err, &nodeErr) || nodeErr.Status != 404 || nodeErr.Code != "not_found" || nodeErr.Message != "participant does not exist" {
		t.Fatalf("want NodeAPIError, got %#v", err)
	}
}

func TestRoomServiceNotLive(t *testing.T) {
	var adminCalls int32
	rs := fakeRoomCloud(t, nil, &adminCalls).RoomService()
	if err := rs.RemoveParticipant(context.Background(), "demo", "a"); !errors.Is(err, ErrRoomNotLive) {
		t.Fatalf("want ErrRoomNotLive, got %v", err)
	}
	if err := rs.DeleteRoom(context.Background(), "demo"); err != nil {
		t.Fatalf("delete of an empty room: %v", err)
	}
	ps, err := rs.ListParticipants(context.Background(), "demo")
	if err != nil || ps == nil || len(ps) != 0 {
		t.Fatalf("list of an empty room: %v %v", ps, err)
	}
	// An empty node list is not cached: someone may join next.
	if n := atomic.LoadInt32(&adminCalls); n != 3 {
		t.Fatalf("admin token fetched %d times, want 3", n)
	}
}

func TestRoomServiceResults(t *testing.T) {
	node := newStubNode(t, func(method string, _ map[string]any) (int, string) {
		switch method {
		case "MutePublishedTrack":
			return 200, `{"track":{"sid":"TR_1","muted":true}}`
		case "UpdateParticipant":
			return 200, `{"sid":"PA_1","identity":"alice","metadata":"m"}`
		case "UpdateRoomMetadata":
			return 200, `{"sid":"RM_1","name":"demo","metadata":"meta"}`
		}
		return 200, `{}`
	})
	var adminCalls int32
	rs := fakeRoomCloud(t, []string{node.url}, &adminCalls).RoomService()
	ctx := context.Background()
	tr, err := rs.MutePublishedTrack(ctx, "demo", "alice", "TR_1", true)
	if err != nil || !tr.Muted {
		t.Fatalf("mute %+v %v", tr, err)
	}
	p, err := rs.UpdateParticipant(ctx, "demo", "alice", UpdateParticipantOptions{Name: "A", Permission: &ParticipantPermission{CanSubscribe: true}})
	if err != nil || p.SID != "PA_1" || p.Metadata != "m" {
		t.Fatalf("update participant %+v %v", p, err)
	}
	perm := node.last("UpdateParticipant").body["permission"].(map[string]any)
	if perm["canSubscribe"] != true || perm["canPublish"] != false {
		t.Fatalf("permission %v", perm)
	}
	r, err := rs.UpdateRoomMetadata(ctx, "demo", "meta")
	if err != nil || r.SID != "RM_1" || r.Metadata != "meta" {
		t.Fatalf("update room %+v %v", r, err)
	}
	// A single-node room needs no participant lookup.
	for _, m := range node.methods() {
		if m == "ListParticipants" {
			t.Fatalf("unexpected lookup: %v", node.methods())
		}
	}
}

func TestCamelizeJSON(t *testing.T) {
	out, err := camelizeJSON([]byte(`{"track_sid":"x","nested":[{"mime_type":"a","big":12345678901234567}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"nested":[{"big":12345678901234567,"mimeType":"a"}],"trackSid":"x"}` {
		t.Fatalf("camelized %s", out)
	}
}
