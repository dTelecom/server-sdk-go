# dTelecom Go server SDK

Use this SDK from your Go backend with [dTelecom Cloud](https://cloud.dtelecom.org): participant tokens,
room administration, webhooks and the hosted AI services, in the `cloud` package.

**Documentation:** [docs.dtelecom.org](https://docs.dtelecom.org), see the
[server SDK reference](https://docs.dtelecom.org/reference/server-sdk/) and the
[quickstart](https://docs.dtelecom.org/quickstart/).

## dTelecom Cloud

The `cloud` package is the Go client of dTelecom Cloud, the same API as `@dtelecom/server-sdk-js` 4.2:
participant tokens, room administration, live speech sessions, file transcription, WAV synthesis,
translation, vision, moderation and node webhooks. It needs Go 1.18+ and no `replace` directive.

```shell
go get github.com/dtelecom/server-sdk-go@latest
```

```go
import "github.com/dtelecom/server-sdk-go/cloud"

// Reads DTELECOM_API_KEY, DTELECOM_API_SECRET and DTELECOM_CLOUD_URL when the options are empty.
c, err := cloud.New(cloud.Options{})

// A token for a client SDK. TTL defaults to 6 hours, at most 24.
tok, err := c.CreateToken(ctx, cloud.TokenOptions{
	Room:     "demo",
	Identity: "alice",
	ClientIP: userIP, // picks the node nearest to the user
})
// give tok.Token and tok.WsURL to the client; tok.Region is the node's city or country

// Extend before tok.ExpiresAt, or the participant is removed about a minute after it.
ext, err := c.ExtendToken(ctx, tok.SessionID, 30*time.Minute)

// What a session was charged (set when it ends: check Final), or a whole room's sessions.
s, err := c.GetSession(ctx, tok.SessionID)
list, err := c.ListSessions(ctx, "demo", cloud.ListSessionsOptions{Since: callStart})
```

Errors from the cloud are `*cloud.APIError` with `Status`, `Code`, `RetryAfter`, `ID` and `Detail`:

```go
if cloud.IsCode(err, "concurrency_limit") { ... }
```

### Rooms

```go
rs := c.RoomService() // keep it: it caches admin tokens
participants, err := rs.ListParticipants(ctx, "demo")
err = rs.RemoveParticipant(ctx, "demo", "bob")
_, err = rs.MutePublishedTrack(ctx, "demo", "alice", trackSID, true)
err = rs.SendData(ctx, "demo", []byte("hello"), cloud.DataReliable, cloud.SendDataOptions{Topic: "chat"})
err = rs.DeleteRoom(ctx, "demo")
```

Calls on a room nobody has joined return `cloud.ErrRoomNotLive`; a node's refusal is a `*cloud.NodeAPIError`.

### A Go bot in a Cloud room

The bot client in this module joins with a Cloud token:

```go
tok, _ := c.CreateToken(ctx, cloud.TokenOptions{Room: "demo", Identity: "bot"})
room, err := lksdk.ConnectToRoomWithToken([]string{tok.WsURL}, tok.Token, lksdk.NewRoomCallback())
```

### AI services

```go
// Translation, vision and moderation: Running means call again with the same key, or poll.
tr, err := c.Translate(ctx, cloud.TranslateOptions{Q: "Good morning", Target: "de", IdempotencyKey: "tr-1"})

// Speech sessions: open StreamURL and send Token as session_key. One key, one connection.
stt, err := c.STT.CreateSession(ctx, cloud.STTSessionOptions{Duration: 10 * time.Minute, Language: "en"})

// Files: create, poll Get until JobCompleted, then read the result.
job, err := c.Transcriptions.Create(ctx, cloud.TranscriptionOptions{File: f, Filename: "call.mp3", IdempotencyKey: "tx-1"})
wav, err := c.WAV.Create(ctx, cloud.WAVOptions{Text: "Hello", IdempotencyKey: "wav-1"})
audio, err := c.WAV.Download(ctx, wav.ID) // after completion; close it

v, err := c.Vision.Snapshot(ctx, cloud.VisionOptions{Image: jpeg, Question: "What is on the screen?", IdempotencyKey: "v-1"})
m, err := c.Moderation.Audio(ctx, cloud.ModerateAudioOptions{Audio: pcm16k, IdempotencyKey: "m-1"})

// Moderation over one WebSocket, one item at a time.
stream, err := c.Moderation.Stream(ctx)
res, err := stream.SendVideoFrame(ctx, "frame-1", jpeg)
```

### Webhooks

Webhook URLs are set per participant in `TokenOptions.WebhookURLs` (HTTPS only). The receiver
checks the node's signature against the node registry; it needs outbound access to a Solana RPC.

```go
receiver, err := cloud.NewWebhookReceiver(cloud.WebhookOptions{})

http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
	ev, err := receiver.ReceiveRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	// ev.Event: participant_joined, participant_left, track_published or track_unpublished.
	// Each event is sent once per URL, without retries; ev.ID is the same for every URL.
})
```

### Tests against a cloud

```shell
DTELECOM_API_KEY=... DTELECOM_API_SECRET=... DTELECOM_CLOUD_URL=https://... \
  go test -tags integration -run TestLive -v ./cloud
```

The rest of this README covers the self-hosted API: tokens signed locally with a node key, and the
bot client.

## Installation

```shell
go get github.com/dtelecom/server-sdk-go
```

Note: since v1.0 release, this package requires Go 1.18+ in order to build.

## Token creation

```go
import (
	"time"

	lksdk "github.com/dtelecom/server-sdk-go"
	"github.com/dtelecom/protocol/auth"
)

func getJoinToken(apiKey, apiSecret, room, identity string) (string, error) {
	at := auth.NewAccessToken(apiKey, apiSecret)
	grant := &auth.VideoGrant{
		RoomJoin: true,
		Room:     room,
	}
	at.AddGrant(grant).
		SetIdentity(identity).
		SetValidFor(time.Hour)

	return at.ToJWT()
}
```

## RoomService API

RoomService gives you complete control over rooms and participants within them. It includes selective track subscriptions as well as moderation capabilities.

```go
import (
	lksdk "github.com/dtelecom/server-sdk-go"
	livekit "github.com/dtelecom/protocol/livekit"
)

func main() {
	host := "host"
	apiKey := "key"
	apiSecret := "secret"

	roomName := "myroom"
	identity := "participantIdentity"

    roomClient := lksdk.NewRoomServiceClient(host, apiKey, apiSecret)

    // create a new room
    room, _ := roomClient.CreateRoom(context.Background(), &livekit.CreateRoomRequest{
		Name: roomName,
	})

    // list rooms
    res, _ := roomClient.ListRooms(context.Background(), &livekit.ListRoomsRequest{})

    // terminate a room and cause participants to leave
    roomClient.DeleteRoom(context.Background(), &livekit.DeleteRoomRequest{
		Room: roomId,
	})

    // list participants in a room
    res, _ := roomClient.ListParticipants(context.Background(), &livekit.ListParticipantsRequest{
		Room: roomName,
	})

    // disconnect a participant from room
    roomClient.RemoveParticipant(context.Background(), &livekit.RoomParticipantIdentity{
		Room:     roomName,
		Identity: identity,
	})

    // mute/unmute participant's tracks
    roomClient.MutePublishedTrack(context.Background(), &livekit.MuteRoomTrackRequest{
		Room:     roomName,
		Identity: identity,
		TrackSid: "track_sid",
		Muted:    true,
	})
}
```

## Interacting as a participant

The Participant SDK gives you access programmatic access as a client enabling you to publish and record audio/video/data to the room.

```go
import (
  lksdk "github.com/dtelecom/server-sdk-go"
)

func main() {
  host := "<host>"
  apiKey := "api-key"
  apiSecret := "api-secret"
  roomName := "myroom"
  identity := "botuser"
  roomCB := &lksdk.RoomCallback{
	ParticipantCallback: lksdk.ParticipantCallback{
	  OnTrackSubscribed: trackSubscribed
  	},
  }
  room, err := lksdk.ConnectToRoom(host, lksdk.ConnectInfo{
  	APIKey:              apiKey,
  	APISecret:           apiSecret,
  	RoomName:            roomName,
  	ParticipantIdentity: identity,
  }, roomCB)
  if err != nil {
  	panic(err)
  }

  ...
  room.Disconnect()
}

func trackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {

}
```

## Publishing tracks to Room

With the Go SDK, you can publish existing files encoded in H.264, VP8, and Opus to the room.

First, you will need to encode media into the right format.

### VP8 / Opus

```bash
ffmpeg -i <input.mp4> \
  -c:v libvpx -keyint_min 120 -qmax 50 -maxrate 2M -b:v 1M <output.ivf> \
  -c:a libopus -page_duration 20000 -vn <output.ogg>
```

The above encodes VP8 at average 1Mbps / max 2Mbps with a minimum keyframe interval of 120.

### H.264 / Opus

```bash
ffmpeg -i <input.mp4> \
  -c:v libx264 -bsf:v h264_mp4toannexb -b:v 2M -profile baseline -pix_fmt yuv420p \
    -x264-params keyint=120 -max_delay 0 -bf 0 <output.h264> \
  -c:a libopus -page_duration 20000 -vn <output.ogg>
```

The above encodes H264 with CBS of 2Mbps with a minimum keyframe interval of 120.

### Publish from file

```go
file := "video.ivf"
videoWidth := 1920
videoHeight := 1080
track, err := lksdk.NewLocalFileTrack(file,
	// control FPS to ensure synchronization
	lksdk.ReaderTrackWithFrameDuration(33 * time.Millisecond),
	lksdk.ReaderTrackWithOnWriteComplete(func() { fmt.Println("track finished") }),
)
if err != nil {
    return err
}
if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
	Name: file,
	Width: videoWidth,
	Height: videoHeight,
}); err != nil {
    return err
}
```

### Publish from io.ReadCloser implementation

```go
// - `in` implements io.ReadCloser, such as buffer or file
// - `mime` has to be one of webrtc.MimeType...
track, err := lksdk.NewLocalReaderTrack(in, mime,
	lksdk.ReaderTrackWithFrameDuration(33 * time.Millisecond),
	lksdk.ReaderTrackWithOnWriteComplete(func() { fmt.Println("track finished") }),
)
if err != nil {
	return err
}
if _, err = room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{}); err != nil {
    return err
}
```

### Publish from other sources

In order to publish from non-file sources, you will have to implement your own `SampleProvider`, that could provide frames of data with a `NextSample` method.

The SDK takes care of sending the samples to the room.

## Receiving webhooks

The Go SDK helps you to verify and decode webhook callbacks to ensure their authenticity.
For dTelecom Cloud webhooks see [Webhooks](https://docs.dtelecom.org/rooms/webhooks/) and the `cloud` package above.

```go
import (
	"github.com/dtelecom/protocol/auth"
	"github.com/dtelecom/protocol/livekit"
	"github.com/dtelecom/protocol/webhook"
)

var authProvider = auth.NewSimpleKeyProvider(
	apiKey, apiSecret,
)

func ServeHTTP(w http.ResponseWriter, r *http.Request) {
  // event is a livekit.WebhookEvent{} object
	event, err := webhook.ReceiveWebhookEvent(r, authProvider)
	if err != nil {
		// could not validate, handle error
		return
	}

	// consume WebhookEvent
}
```
