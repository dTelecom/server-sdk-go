package lksdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestJoinCandidates(t *testing.T) {
	for _, tt := range []struct {
		name     string
		status   int
		wantNext bool
	}{
		{"unavailable", 503, true},
		{"timeout", 408, true},
		{"rate limit", 429, true},
		{"unauthorized", 401, false},
		{"forbidden", 403, false},
		{"bad request", 400, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var order []string
			node := func(name string, status int) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					order = append(order, name)
					mu.Unlock()
					if r.URL.Path != "/rtc" || r.Header.Get("Authorization") != "Bearer room-token" {
						t.Errorf("unexpected signaling request")
					}
					w.WriteHeader(status)
				}))
			}
			a := node("A", tt.status)
			defer a.Close()
			b := node("B", 403)
			defer b.Close()
			_, err := ConnectToRoomWithToken([]string{a.URL, a.URL + "/", ToWebsocketURL(a.URL), b.URL}, "room-token", nil)
			require.Error(t, err)
			mu.Lock()
			defer mu.Unlock()
			if tt.wantNext {
				require.Equal(t, []string{"A", "B"}, order)
			} else {
				require.Equal(t, []string{"A"}, order)
			}
		})
	}
}

func TestRestartCandidatesPreferServingNode(t *testing.T) {
	var p serverPool
	p.Init([]string{"https://A:443", "wss://b", "wss://c", "wss://a/", "http://downgrade"})
	p.connected("wss://c")
	require.Equal(t, []string{"wss://c", "https://A:443", "wss://b"}, p.candidates())
	require.Equal(t, "wss://c", p.Current())
}

func TestFailedMediaAttemptClosesBeforeNextJoin(t *testing.T) {
	upgrader := websocket.Upgrader{}
	closed := make(chan struct{})
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		defer close(closed)
		frame, _ := proto.Marshal(&livekit.SignalResponse{Message: &livekit.SignalResponse_Join{Join: &livekit.JoinResponse{SubscriberPrimary: true}}})
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Error("preceding node's signal socket left open")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer b.Close()
	e := NewRTCEngine()
	defer e.Close()
	e.JoinTimeout = 200 * time.Millisecond
	_, err := e.JoinWithServers([]string{a.URL, b.URL}, "room-token", &ConnectParams{})
	require.ErrorContains(t, err, "403")
	require.Nil(t, e.GetDataChannel(livekit.DataPacket_RELIABLE))
}

func TestCancelJoinStopsFallback(t *testing.T) {
	entered := make(chan struct{})
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("contacted alternative after cancellation")
	}))
	defer b.Close()
	e := NewRTCEngine()
	result := make(chan error, 1)
	go func() {
		_, err := e.JoinWithServers([]string{a.URL, b.URL}, "room-token", &ConnectParams{})
		result <- err
	}()
	<-entered
	e.Close()
	select {
	case err := <-result:
		require.True(t, errors.Is(err, context.Canceled))
	case <-time.After(2 * time.Second):
		t.Fatal("pending join did not cancel")
	}
}

// A full restart negotiates a new peer connection on the serving node first.
// A reachability failure then returns to the issuer's preference order.
func TestFullRestartRejoinsServingNodeFirst(t *testing.T) {
	seen := make(chan string, 3)
	node := func(name string, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen <- name
			w.WriteHeader(status)
		}))
	}
	a := node("A", http.StatusUnauthorized)
	defer a.Close()
	b := node("B", http.StatusServiceUnavailable)
	defer b.Close()
	e := NewRTCEngine()
	defer e.Close()
	e.connParams = &ConnectParams{}
	e.serverPool.Init([]string{a.URL, b.URL})
	e.serverPool.connected(b.URL)
	require.ErrorContains(t, e.restartConnection(), "401")
	require.Equal(t, "B", <-seen)
	require.Equal(t, "A", <-seen)
	require.Empty(t, seen)
}
