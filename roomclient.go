package lksdk

import (
	"context"
	"net/http"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

type RoomServiceClient struct {
	roomService livekit.RoomService
	authBase
}

func NewRoomServiceClient(url string, apiKey string, secretKey string) *RoomServiceClient {
	url = ToHttpURL(url)
	client := livekit.NewRoomServiceProtobufClient(url, &http.Client{})
	return &RoomServiceClient{
		roomService: client,
		authBase: authBase{
			apiKey:    apiKey,
			apiSecret: secretKey,
		},
	}
}

func (c *RoomServiceClient) DeleteRoom(ctx context.Context, req *livekit.DeleteRoomRequest) (*livekit.DeleteRoomResponse, error) {
	ctx, err := c.withAuth(ctx, auth.VideoGrant{RoomCreate: true})
	if err != nil {
		return nil, err
	}

	return c.roomService.DeleteRoom(ctx, req)
}

func (c *RoomServiceClient) RemoveParticipant(ctx context.Context, req *livekit.RoomParticipantIdentity) (*livekit.RemoveParticipantResponse, error) {
	ctx, err := c.withAuth(ctx, auth.VideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.roomService.RemoveParticipant(ctx, req)
}

func (c *RoomServiceClient) MutePublishedTrack(ctx context.Context, req *livekit.MuteRoomTrackRequest) (*livekit.MuteRoomTrackResponse, error) {
	ctx, err := c.withAuth(ctx, auth.VideoGrant{RoomAdmin: true, Room: req.Room})
	if err != nil {
		return nil, err
	}

	return c.roomService.MutePublishedTrack(ctx, req)
}

func (c *RoomServiceClient) CreateToken() *auth.AccessToken {
	return auth.NewAccessToken(c.apiKey, c.apiSecret)
}
