package cloud

import (
	"context"
	"errors"
	"testing"
	"time"
)

const sttSessionJSON = `{"session_id":"stt-1","service":"stt","status":"completed","room_name":"call","identity":"alice",
"webrtc_session_id":"s1","created_at":"2030-01-01T00:00:05Z","ended_at":"2030-01-01T00:01:05Z",
"charged_microcredits":"4000","charged_usd":"0.004","final":true,"covered_by_addon":false}`

func TestGetSession(t *testing.T) {
	f, c := newFakeCloud(t, 200, sttSessionJSON, nil)
	s, err := c.GetSession(context.Background(), "stt-1")
	if err != nil {
		t.Fatal(err)
	}
	if f.method != "GET" || f.path != "/v1/sessions/stt-1" {
		t.Fatalf("request %s %s", f.method, f.path)
	}
	if s.Service != "stt" || s.WebRTCSessionID != "s1" || s.RoomName != "call" || s.ChargedMicrocredits != "4000" ||
		s.ChargedUSD != "0.004" || !s.Final || s.StartedAt != nil || s.EndedAt == nil ||
		!s.CreatedAt.Equal(time.Date(2030, 1, 1, 0, 0, 5, 0, time.UTC)) {
		t.Fatalf("session %+v", s)
	}

	_, c2 := newFakeCloud(t, 404, `{"error":"session_not_found"}`, nil)
	var apiErr *APIError
	if _, err := c2.GetSession(context.Background(), "none"); !errors.As(err, &apiErr) || apiErr.Code != "session_not_found" {
		t.Fatalf("err %v", err)
	}
}

func TestListSessions(t *testing.T) {
	f, c := newFakeCloud(t, 200, `{"sessions":[`+sttSessionJSON+`],"total_microcredits":"4000","total_usd":"0.004"}`, nil)
	list, err := c.ListSessions(context.Background(), "my call", ListSessionsOptions{Since: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if f.path != "/v1/sessions" || f.query != "limit=10&room=my+call&since=2030-01-01T00%3A00%3A00Z" {
		t.Fatalf("request %s?%s", f.path, f.query)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != "stt-1" || list.TotalUSD != "0.004" || list.TotalMicrocredits != "4000" {
		t.Fatalf("list %+v", list)
	}

	f2, c2 := newFakeCloud(t, 200, `{"sessions":null,"total_microcredits":"0","total_usd":"0"}`, nil)
	empty, err := c2.ListSessions(context.Background(), "r", ListSessionsOptions{})
	if err != nil || empty.Sessions == nil || len(empty.Sessions) != 0 || f2.query != "room=r" {
		t.Fatalf("empty %+v %v %s", empty, err, f2.query)
	}
}
