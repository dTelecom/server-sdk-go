package lksdk

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"go.uber.org/atomic"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
)

const PROTOCOL = 8

type SignalClient struct {
	conn            atomic.Value // *websocket.Conn
	lock            sync.Mutex
	isClosed        atomic.Bool
	isStarted       atomic.Bool
	pendingResponse *livekit.SignalResponse
	workerMu        sync.Mutex
	workerDone      chan struct{}

	OnClose                 func()
	OnAnswer                func(sd webrtc.SessionDescription)
	OnOffer                 func(sd webrtc.SessionDescription)
	OnTrickle               func(init webrtc.ICECandidateInit, target livekit.SignalTarget)
	OnParticipantUpdate     func([]*livekit.ParticipantInfo)
	OnLocalTrackPublished   func(response *livekit.TrackPublishedResponse)
	OnSpeakersChanged       func([]*livekit.SpeakerInfo)
	OnConnectionQuality     func([]*livekit.ConnectionQualityInfo)
	OnRoomUpdate            func(room *livekit.Room)
	OnTrackMuted            func(request *livekit.MuteTrackRequest)
	OnLocalTrackUnpublished func(response *livekit.TrackUnpublishedResponse)
	OnTokenRefresh          func(refreshToken string)
	OnLeave                 func(*livekit.LeaveRequest)
}

func NewSignalClient() *SignalClient {
	c := &SignalClient{}
	return c
}

func (c *SignalClient) Start() {
	c.workerMu.Lock()
	defer c.workerMu.Unlock()
	if c.websocketConn() == nil || c.isClosed.Load() || c.isStarted.Swap(true) {
		return
	}
	c.workerDone = make(chan struct{})
	go c.readWorker(c.workerDone)
}

func (c *SignalClient) IsStarted() bool {
	return c.isStarted.Load()
}

func (c *SignalClient) Join(urlPrefix string, token string, params *ConnectParams) (*livekit.JoinResponse, error) {
	return c.join(context.Background(), urlPrefix, token, params, 15*time.Second)
}

func (c *SignalClient) join(ctx context.Context, urlPrefix, token string, params *ConnectParams, timeout time.Duration) (*livekit.JoinResponse, error) {
	if urlPrefix == "" {
		return nil, ErrURLNotProvided
	}
	if params == nil {
		return nil, ErrInvalidParameter
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c.Close()
	// The previous reader must finish before its callbacks and socket can be replaced.
	c.workerMu.Lock()
	done := c.workerDone
	c.workerMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	u, err := url.Parse(ToWebsocketURL(urlPrefix))
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
		return nil, ErrInvalidParameter
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/rtc"
	q := u.Query()
	q.Set("protocol", fmt.Sprint(PROTOCOL))
	q.Set("sdk", "go")
	q.Set("version", Version)
	if params.AutoSubscribe {
		q.Set("auto_subscribe", "1")
	} else {
		q.Set("auto_subscribe", "0")
	}
	if params.Reconnect {
		q.Set("reconnect", "1")
	}
	u.RawQuery = q.Encode()

	header := newHeaderWithToken(token)
	dialer, stopWatching := signalDialer(ctx)
	defer stopWatching()
	conn, response, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if response != nil {
			response.Body.Close()
			status := response.StatusCode
			// The Go dialer exposes the actual authenticated upgrade response.
			if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
				return nil, fmt.Errorf("%w: HTTP %d", ErrCannotConnectSignal, status)
			}
			return nil, fmt.Errorf("signal connection rejected: HTTP %d", status)
		}
		if ctx.Err() != nil {
			return nil, classifySignalError(ctx.Err())
		}
		return nil, classifySignalError(err)
	}
	c.conn.Store(conn)
	c.isClosed.Store(false)
	if err := ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}

	deadline, _ := ctx.Deadline()
	conn.SetReadDeadline(deadline)
	res, err := readSignalResponse(conn)
	if err != nil {
		c.Close()
		if ctx.Err() != nil {
			return nil, classifySignalError(ctx.Err())
		}
		return nil, classifySignalError(err)
	}
	conn.SetReadDeadline(time.Time{})
	c.pendingResponse = nil
	if params.Reconnect {
		logger.Debugw("reconnect received response", "type", fmt.Sprintf("%T", res.Message))
		if res != nil {
			if res.GetLeave() != nil {
				c.Close()
				if res.GetLeave().GetCanReconnect() || res.GetLeave().GetReason() == livekit.DisconnectReason_STATE_MISMATCH {
					return nil, fmt.Errorf("%w: reconnect refused (%s)", ErrCannotConnectSignal, res.GetLeave().GetReason())
				}
				return nil, fmt.Errorf("reconnect refused (%s)", res.GetLeave().GetReason())
			}
			c.pendingResponse = res
		}
		return nil, nil
	}

	join := res.GetJoin()
	if join == nil {
		c.Close()
		return nil, fmt.Errorf("unexpected join response: %T", res.Message)
	}

	return join, nil
}

func (c *SignalClient) Close() {
	if c.isClosed.Swap(true) {
		return
	}
	if conn := c.websocketConn(); conn != nil {
		_ = conn.Close()
	}
}

func (c *SignalClient) SendICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Trickle{
			Trickle: ToProtoTrickle(candidate, target),
		},
	})
}

func (c *SignalClient) SendOffer(sd webrtc.SessionDescription) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Offer{
			Offer: ToProtoSessionDescription(sd),
		},
	})
}

func (c *SignalClient) SendAnswer(sd webrtc.SessionDescription) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Answer{
			Answer: ToProtoSessionDescription(sd),
		},
	})
}

func (c *SignalClient) SendMuteTrack(sid string, muted bool) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Mute{
			Mute: &livekit.MuteTrackRequest{
				Sid:   sid,
				Muted: muted,
			},
		},
	})
}

func (c *SignalClient) SendSyncState(state *livekit.SyncState) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_SyncState{
			SyncState: state,
		},
	})
}

func (c *SignalClient) SendLeave() error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_Leave{
			Leave: &livekit.LeaveRequest{},
		},
	})
}

func (c *SignalClient) SendRequest(req *livekit.SignalRequest) error {
	conn := c.websocketConn()
	if conn == nil {
		return errors.New("client is not connected")
	}
	payload, err := proto.Marshal(req)
	if err != nil {
		return err
	}

	c.lock.Lock()
	defer c.lock.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, payload)
}

func (c *SignalClient) SendUpdateTrackSettings(settings *livekit.UpdateTrackSettings) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_TrackSetting{
			TrackSetting: settings,
		},
	})
}

func (c *SignalClient) SendUpdateParticipantMetadata(metadata *livekit.UpdateParticipantMetadata) error {
	return c.SendRequest(&livekit.SignalRequest{
		Message: &livekit.SignalRequest_UpdateMetadata{
			UpdateMetadata: metadata,
		},
	})
}

// signalDialer binds pending TCP reads (including the HTTP upgrade) to the
// caller's context. Stop watching once the handshake completes so a successful
// WebSocket outlives its join context.
func signalDialer(ctx context.Context) (websocket.Dialer, func()) {
	dialer := *websocket.DefaultDialer
	stop := make(chan struct{})
	var readers sync.WaitGroup

	watch := func(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
		return func(dialCtx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(dialCtx, network, address)
			if err == nil {
				readers.Add(1)
				go func() {
					defer readers.Done()
					select {
					case <-stop:
					case <-ctx.Done():
						conn.Close()
					}
				}()
			}
			return conn, err
		}
	}
	tcpDial := dialer.NetDialContext
	if tcpDial == nil {
		if dialer.NetDial != nil {
			tcpDial = func(_ context.Context, network, address string) (net.Conn, error) {
				return dialer.NetDial(network, address)
			}
		} else {
			tcpDial = (&net.Dialer{}).DialContext
		}
	}
	dialer.NetDialContext = watch(tcpDial)
	if dialer.NetDialTLSContext != nil {
		dialer.NetDialTLSContext = watch(dialer.NetDialTLSContext)
	}
	return dialer, func() { close(stop); readers.Wait() }
}

func classifySignalError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	var authority x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &authority) || errors.As(err, &host) || errors.As(err, &invalid) {
		return err
	}
	var networkError net.Error
	if errors.As(err, &networkError) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		websocket.IsCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
		return fmt.Errorf("%w: %v", ErrCannotConnectSignal, err)
	}
	return err
}

func readSignalResponse(conn *websocket.Conn) (*livekit.SignalResponse, error) {
	// handle special messages and pass on the rest
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	msg := &livekit.SignalResponse{}
	switch messageType {
	case websocket.BinaryMessage:
		// protobuf encoded
		err := proto.Unmarshal(payload, msg)
		return msg, err
	case websocket.TextMessage:
		// json encoded
		err := protojson.Unmarshal(payload, msg)
		return msg, err
	default:
		return nil, errors.New("unsupported signaling message")
	}
}

func (c *SignalClient) handleResponse(res *livekit.SignalResponse) {
	switch msg := res.Message.(type) {
	case *livekit.SignalResponse_Answer:
		if c.OnAnswer != nil {
			c.OnAnswer(FromProtoSessionDescription(msg.Answer))
		}
	case *livekit.SignalResponse_Offer:
		if c.OnOffer != nil {
			c.OnOffer(FromProtoSessionDescription(msg.Offer))
		}
	case *livekit.SignalResponse_Trickle:
		if c.OnTrickle != nil {
			c.OnTrickle(FromProtoTrickle(msg.Trickle), msg.Trickle.Target)
		}
	case *livekit.SignalResponse_Update:
		if c.OnParticipantUpdate != nil {
			c.OnParticipantUpdate(msg.Update.Participants)
		}
	case *livekit.SignalResponse_SpeakersChanged:
		if c.OnSpeakersChanged != nil {
			c.OnSpeakersChanged(msg.SpeakersChanged.Speakers)
		}
	case *livekit.SignalResponse_TrackPublished:
		if c.OnLocalTrackPublished != nil {
			c.OnLocalTrackPublished(msg.TrackPublished)
		}
	case *livekit.SignalResponse_Mute:
		if c.OnTrackMuted != nil {
			c.OnTrackMuted(msg.Mute)
		}
	case *livekit.SignalResponse_ConnectionQuality:
		if c.OnConnectionQuality != nil {
			c.OnConnectionQuality(msg.ConnectionQuality.Updates)
		}
	case *livekit.SignalResponse_RoomUpdate:
		if c.OnRoomUpdate != nil {
			c.OnRoomUpdate(msg.RoomUpdate.Room)
		}
	case *livekit.SignalResponse_Leave:
		if c.OnLeave != nil {
			c.OnLeave(msg.Leave)
		}
	case *livekit.SignalResponse_RefreshToken:
		if c.OnTokenRefresh != nil {
			c.OnTokenRefresh(msg.RefreshToken)
		}
	case *livekit.SignalResponse_TrackUnpublished:
		if c.OnLocalTrackUnpublished != nil {
			c.OnLocalTrackUnpublished(msg.TrackUnpublished)
		}
	}
}

func (c *SignalClient) readWorker(done chan struct{}) {
	conn := c.websocketConn()
	defer func() {
		c.isStarted.Store(false)
		intentional := c.isClosed.Load()
		close(done)
		if !intentional && c.OnClose != nil {
			c.OnClose()
		}
	}()
	if pending := c.pendingResponse; pending != nil {
		c.handleResponse(pending)
		c.pendingResponse = nil
	}
	for !c.isClosed.Load() {
		res, err := readSignalResponse(conn)
		if err != nil {
			if !isIgnoredWebsocketError(err) && !c.isClosed.Load() {
				logger.Infow("error while reading from signal client", "err", err)
			}
			return
		}
		c.handleResponse(res)
	}
}

func (c *SignalClient) websocketConn() *websocket.Conn {
	obj := c.conn.Load()
	if obj == nil {
		return nil
	}
	if conn, ok := obj.(*websocket.Conn); ok {
		return conn
	}
	return nil
}

func isIgnoredWebsocketError(err error) bool {
	if err == nil || err == io.EOF {
		return true
	}

	return websocket.IsCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseGoingAway, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived)
}
