package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	lksdk "github.com/dtelecom/server-sdk-go"
	"github.com/go-logr/stdr"
	protoLogger "github.com/livekit/protocol/logger"
	"github.com/pion/webrtc/v3"
)

var (
	url, apiKey, apiSecret, roomName, identity string
	outPath                                     string
)

var logger protoLogger.Logger = protoLogger.LogRLogger(stdr.New(log.Default()))

func init() {
	flag.StringVar(&url, "url", "", "livekit server url")
	flag.StringVar(&apiKey, "api-key", "", "livekit api key")
	flag.StringVar(&apiSecret, "api-secret", "", "livekit api secret")
	flag.StringVar(&roomName, "room-name", "", "room name")
	flag.StringVar(&identity, "identity", "", "participant identity")
	flag.StringVar(&outPath, "out", "", "output file path for raw keyframe")
}

func main() {
	flag.Parse()

	url = "ws://localhost:7880"
	apiKey = "4sxGjTwxMHePA3X5gsi42chui91YxN2juoxXwM3fXP2Q"
	apiSecret = "2ezR1fmGT8NNNFWTMoTVmjtdZ6yzcQBDfY7MDpKcTup5yHLD1tjn7QVFTJm7XLGv4kAVRMb4hZwsLm4iTSLx4GME"
	roomName = "room1"
	identity = "user2"

	outPath = "/Users/vsitnev/Desktop/dTelecom/server-sdk-go/examples/screenshot/screenshot.webp"

	if url == "" || apiKey == "" || apiSecret == "" || roomName == "" || identity == "" {
		log.Printf("invalid arguments: need -host, -api-key, -api-secret, -room-name, -identity")
		return
	}

	done := make(chan struct{})

	room, err := lksdk.ConnectToRoom(url, lksdk.ConnectInfo{
		APIKey:              apiKey,
		APISecret:           apiSecret,
		RoomName:            roomName,
		ParticipantIdentity: identity,
	}, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				go func() {
					mime := track.Codec().MimeType
					logger.Infow("subscribed to track", "id", track.ID(), "codec", mime, "participant", rp.Identity())

					if mime != webrtc.MimeTypeVP8 && mime != webrtc.MimeTypeH264 {
						logger.Errorw("unsupported codec for screenshot", fmt.Errorf("%s", mime))
						return
					}

					defer func() {
						select {
						case <-done:
						default:
							close(done)
						}
					}()

					data, err := lksdk.MakeScreenShot(track, mime)
					if err != nil {
						logger.Errorw("failed to make screenshot", err)
						return
					}

					if len(data) == 0 {
						logger.Errorw("screenshot is empty", nil)
						return
					}

					if mime == webrtc.MimeTypeVP8 {
						webpData, err := vp8KeyframeToWebP(data)
						if err != nil {
							logger.Errorw("failed to convert VP8 keyframe to WebP", err)
							return
						}
						data = webpData
					} else {
						logger.Warnw("codec is not VP8, saving raw keyframe as-is (not valid WebP)", nil)
					}

					f, err := os.Create(outPath)
					if err != nil {
						logger.Errorw("failed to create file", err)
						return
					}
					defer f.Close()

					if _, err := f.Write(data); err != nil {
						logger.Errorw("failed to write screenshot", err)
						return
					}

					logger.Infow("screenshot saved", "path", outPath, "size", len(data), "codec", mime)
				}()
			},
		},
	})
	if err != nil {
		logger.Errorw("failed to connect to room", err)
		return
	}

	timeout := 20 * time.Second
	select {
	case <-done:
		logger.Infow("screenshot flow finished")
	case <-time.After(timeout):
		logger.Errorw("timeout waiting for video track / screenshot", fmt.Errorf("%s", timeout.String()))
	}

	room.Disconnect()
	log.Printf("room disconnected, exit")
}

func vp8KeyframeToWebP(vp8Frame []byte) ([]byte, error) {
	chunkSize := len(vp8Frame)

	paddedChunkSize := chunkSize
	if paddedChunkSize%2 == 1 {
		paddedChunkSize++
	}

	riffSize := uint32(4 + 8 + paddedChunkSize)

	buf := &bytes.Buffer{}

	buf.WriteString("RIFF")
	if err := binary.Write(buf, binary.LittleEndian, riffSize); err != nil {
		return nil, err
	}
	buf.WriteString("WEBP")

	buf.WriteString("VP8 ")
	if err := binary.Write(buf, binary.LittleEndian, uint32(chunkSize)); err != nil {
		return nil, err
	}
	buf.Write(vp8Frame)

	if chunkSize%2 == 1 {
		buf.WriteByte(0)
	}

	return buf.Bytes(), nil
}