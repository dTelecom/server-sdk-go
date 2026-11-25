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

### Manage a live room

The first participant to join starts the room. Obtain a room service with `c.RoomService()` and
pass the room name to `ListParticipants(ctx, roomName)`. The service locates the nodes serving that room and caches its admin tokens.
To list participants, set your Cloud credentials in the environment and run the
[room-management example](examples/room-management/main.go) from this checkout while the call is active.

```shell
go run ./examples/room-management my-room
```

The example prints participant identities. If the room has no serving nodes, the list is empty.
The service also supports looking up a participant, muting a published track, sending data,
removing a participant and ending the call. For an application that needs a room directory, keep
the room names in your backend. See the [room-management guide](https://docs.dtelecom.org/rooms/manage-rooms/).

### Join a Cloud room from Go

Use the root package when your process is itself a WebRTC participant. Import it as
`lksdk "github.com/dtelecom/server-sdk-go"` alongside the `cloud` package.

```go
func join(ctx context.Context, client *cloud.Client, roomName, identity string) (*lksdk.Room, error) {
    token, err := client.CreateToken(ctx, cloud.TokenOptions{Room: roomName, Identity: identity})
    if err != nil {
        return nil, err
    }
    return lksdk.ConnectToRoomWithToken([]string{token.WsURL}, token.Token, lksdk.NewRoomCallback())
}
```

Call `room.Disconnect()` when the participant leaves.

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

### Tests

Run the offline suite with `go test ./...`. CI also builds the room-management example and tests the candidate SDK from a separate Go module. The consumer uses the public protocol version selected by Go.

The **Go SDK tests** workflow offers a manual **live** input on the default branch. It joins two participants using short-lived Cloud tokens, publishes an audio track, exchanges reliable data, checks both identities with the room-management example and exercises the room APIs. The job uses a dedicated test project configured in the `sdk-live-tests` environment and sends no requests on pull requests.

To run the same room test locally with ffmpeg installed, export `DTELECOM_API_KEY`, `DTELECOM_API_SECRET` and `DTELECOM_CLOUD_URL` for your test project, then run:

```shell
go test -tags integration -run '^TestLiveRoomsAndBot$' -count=1 ./cloud
```

To also exercise the hosted AI and configured webhook fixtures:

```shell
DTELECOM_API_KEY=... DTELECOM_API_SECRET=... DTELECOM_CLOUD_URL=https://... \
  go test -tags integration -run TestLive -v ./cloud
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
