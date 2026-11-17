//go:build integration

// Live checks against a dTelecom Cloud. They spend a little credit.
//
//	DTELECOM_API_KEY=... DTELECOM_API_SECRET=... DTELECOM_CLOUD_URL=https://... \
//	  go test -tags integration -run TestLive -v ./cloud
//
// TestLiveWebhook also needs WEBHOOK_PUBLIC_URL (an https URL that reaches WEBHOOK_LISTEN, default
// :8099) and the registry the cloud's nodes use (SOLANA_* env vars).
package cloud_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	lksdk "github.com/dtelecom/server-sdk-go"
	"github.com/dtelecom/server-sdk-go/cloud"
	"github.com/livekit/protocol/livekit"
)

func liveClient(t *testing.T) *cloud.Client {
	t.Helper()
	if os.Getenv("DTELECOM_API_KEY") == "" {
		t.Skip("DTELECOM_API_KEY not set")
	}
	// The test project may be on the Free plan (5 requests per second): space the calls.
	c, err := cloud.New(cloud.Options{HTTPClient: &http.Client{Transport: &pacedTransport{gap: 250 * time.Millisecond}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pacedTransport leaves at least gap between the starts of two requests.
type pacedTransport struct {
	mu   sync.Mutex
	last time.Time
	gap  time.Duration
}

func (p *pacedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	if wait := p.gap - time.Since(p.last); wait > 0 {
		time.Sleep(wait)
	}
	p.last = time.Now()
	p.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

func key(prefix string) string { return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()) }

// eventually retries f every 500 ms until it returns nil or the timeout passes.
func eventually(t *testing.T, timeout time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ffmpeg makes a test file, or skips the test when ffmpeg is missing.
func ffmpeg(t *testing.T, name string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found")
	}
	out := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("ffmpeg", append(append([]string{"-loglevel", "error", "-y"}, args...), out)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, b)
	}
	return out
}

type bot struct {
	room     *lksdk.Room
	mu       sync.Mutex
	data     []string
	gone     chan struct{}
	goneOnce sync.Once
}

func (b *bot) received(s string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, d := range b.data {
		if d == s {
			return true
		}
	}
	return false
}

// joinBot joins a Go bot with a Cloud token through the existing bot client.
func joinBot(t *testing.T, tok *cloud.ParticipantToken) *bot {
	t.Helper()
	b := &bot{gone: make(chan struct{})}
	cb := lksdk.NewRoomCallback()
	cb.OnDataReceived = func(data []byte, rp *lksdk.RemoteParticipant) {
		b.mu.Lock()
		b.data = append(b.data, string(data))
		b.mu.Unlock()
	}
	cb.OnDisconnected = func() { b.goneOnce.Do(func() { close(b.gone) }) }
	start := time.Now()
	room, err := lksdk.ConnectToRoomWithToken([]string{tok.WsURL}, tok.Token, cb)
	if err != nil {
		t.Fatalf("bot join %s: %v", tok.WsURL, err)
	}
	t.Logf("bot %s joined %s in %s", room.LocalParticipant.Identity(), tok.WsURL, time.Since(start).Round(time.Millisecond))
	b.room = room
	t.Cleanup(room.Disconnect)
	return b
}

func TestLiveRoomsAndBot(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()
	roomName := key("go-sdk")
	rs := c.RoomService()

	// Before anyone joins there is no node to call.
	adm, err := c.AdminToken(ctx, roomName)
	if err != nil {
		t.Fatal(err)
	}
	if len(adm.APIURLs) != 0 {
		t.Fatalf("empty room has nodes %v", adm.APIURLs)
	}
	if err := rs.RemoveParticipant(ctx, roomName, "nobody"); !errors.Is(err, cloud.ErrRoomNotLive) {
		t.Fatalf("want ErrRoomNotLive, got %v", err)
	}

	aliceTok, err := c.CreateToken(ctx, cloud.TokenOptions{Room: roomName, Identity: "alice", Name: "Alice", Metadata: `{"role":"host"}`, TTL: 10 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	bobTok, err := c.CreateToken(ctx, cloud.TokenOptions{Room: roomName, Identity: "bob", TTL: 10 * time.Minute, Grants: &cloud.Grants{CanPublish: cloud.Bool(false)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tokens: %s (expires %s), node %s", aliceTok.SessionID, aliceTok.ExpiresAt.Format(time.RFC3339), aliceTok.WsURL)

	alice := joinBot(t, aliceTok)
	bob := joinBot(t, bobTok)

	// Alice publishes audio from an Opus file.
	oggPath := ffmpeg(t, "tone.ogg", "-f", "lavfi", "-i", "sine=frequency=440:duration=30", "-c:a", "libopus", "-ar", "48000", "-ac", "1", "-page_duration", "20000")
	track, err := lksdk.NewLocalFileTrack(oggPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := alice.room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{Name: "tone"})
	if err != nil {
		t.Fatalf("publish audio: %v", err)
	}
	t.Logf("alice published audio track %s", pub.SID())

	// Data between the bots, then from the server.
	if err := bob.room.LocalParticipant.PublishData([]byte("from-bob"), livekit.DataPacket_RELIABLE, nil); err != nil {
		t.Fatalf("bob publish data: %v", err)
	}
	eventually(t, 10*time.Second, "alice receives bob's data", func() error {
		if !alice.received("from-bob") {
			return errors.New("not yet")
		}
		return nil
	})
	// The node accepts server data. The root bot client (v1.0.15) drops messages without a sender
	// participant, so alice does not see it; that is a bot-client limitation.
	if err := rs.SendData(ctx, roomName, []byte("from-server"), cloud.DataReliable, cloud.SendDataOptions{Topic: "t"}); err != nil {
		t.Fatalf("SendData: %v", err)
	}
	time.Sleep(2 * time.Second)
	t.Logf("server data accepted by the node; Go bot received it: %v", alice.received("from-server"))

	// Room service.
	var ps []cloud.ParticipantInfo
	eventually(t, 15*time.Second, "list participants", func() error {
		ps, err = rs.ListParticipants(ctx, roomName)
		if err != nil {
			return err
		}
		if len(ps) != 2 {
			return fmt.Errorf("%d participants", len(ps))
		}
		return nil
	})
	var trackSID string
	eventually(t, 15*time.Second, "alice's audio track on the node", func() error {
		p, err := rs.GetParticipant(ctx, roomName, "alice")
		if err != nil {
			return err
		}
		for _, tr := range p.Tracks {
			if tr.Type == "AUDIO" {
				trackSID = tr.SID
				t.Logf("alice on the node: state %s, name %q, metadata %s, track %s %s", p.State, p.Name, p.Metadata, tr.SID, tr.MimeType)
				return nil
			}
		}
		return errors.New("no audio track")
	})
	room, err := rs.UpdateRoomMetadata(ctx, roomName, "room-meta")
	if err != nil {
		t.Fatalf("UpdateRoomMetadata: %v", err)
	}
	t.Logf("UpdateRoomMetadata returned metadata %q (empty from nodes before the fix)", room.Metadata)
	eventually(t, 10*time.Second, "bob sees the room metadata", func() error {
		if bob.room.Metadata() != "room-meta" {
			return fmt.Errorf("metadata %q", bob.room.Metadata())
		}
		return nil
	})
	up, err := rs.UpdateParticipant(ctx, roomName, "alice", cloud.UpdateParticipantOptions{Metadata: "updated"})
	if err != nil {
		t.Fatalf("UpdateParticipant: %v", err)
	}
	t.Logf("UpdateParticipant returned metadata %q (empty from nodes before the fix)", up.Metadata)
	eventually(t, 10*time.Second, "alice's new metadata", func() error {
		p, err := rs.GetParticipant(ctx, roomName, "alice")
		if err != nil {
			return err
		}
		if p.Metadata != "updated" {
			return fmt.Errorf("metadata %q", p.Metadata)
		}
		return nil
	})
	tr, err := rs.MutePublishedTrack(ctx, roomName, "alice", trackSID, true)
	if err != nil || tr == nil || !tr.Muted {
		t.Fatalf("MutePublishedTrack %+v %v", tr, err)
	}
	if err := rs.UpdateSubscriptions(ctx, roomName, "bob", []string{trackSID}, false); err != nil {
		t.Fatalf("UpdateSubscriptions: %v", err)
	}

	ext, err := c.ExtendToken(ctx, aliceTok.SessionID, 10*time.Minute)
	if err != nil || !ext.ExpiresAt.After(aliceTok.ExpiresAt) {
		t.Fatalf("ExtendToken %+v %v", ext, err)
	}

	if err := rs.RemoveParticipant(ctx, roomName, "bob"); err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	select {
	case <-bob.gone:
	case <-time.After(15 * time.Second):
		t.Fatal("bob was not disconnected")
	}
	// A participant who already left: the node answers without an error.
	t.Logf("removing bob again: %v", rs.RemoveParticipant(ctx, roomName, "bob"))
	_, err = rs.GetParticipant(ctx, roomName, "bob")
	var nodeErr *cloud.NodeAPIError
	if !errors.As(err, &nodeErr) {
		t.Fatalf("reading a gone participant: want NodeAPIError, got %v", err)
	}
	t.Logf("reading a gone participant: %d %s %q", nodeErr.Status, nodeErr.Code, nodeErr.Message)

	if err := rs.DeleteRoom(ctx, roomName); err != nil {
		t.Fatalf("DeleteRoom: %v", err)
	}
	select {
	case <-alice.gone:
	case <-time.After(15 * time.Second):
		t.Fatal("alice was not disconnected by DeleteRoom")
	}
}

func TestLiveAIServices(t *testing.T) {
	c := liveClient(t)
	ctx := context.Background()

	stt, err := c.STT.CreateSession(ctx, cloud.STTSessionOptions{Duration: time.Minute, Language: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stt.StreamURL, "/v1/stream") {
		t.Fatalf("stt %+v", stt)
	}
	if _, err := c.STT.Extend(ctx, stt.SessionID, time.Minute); err != nil {
		t.Fatalf("stt extend: %v", err)
	}
	tts, err := c.TTS.CreateSession(ctx, cloud.TTSSessionOptions{MaxCharacters: 100})
	if err != nil {
		t.Fatal(err)
	}
	ext, err := c.TTS.Extend(ctx, tts.SessionID, 50)
	if err != nil || ext.MaxCharacters != 150 {
		t.Fatalf("tts extend %+v %v", ext, err)
	}
	t.Logf("speech sessions: stt %s, tts %s", stt.StreamURL, tts.StreamURL)

	tr, err := c.Translate(ctx, cloud.TranslateOptions{Q: "Good morning", Target: "de", IdempotencyKey: key("tr")})
	for err == nil && tr.Status == cloud.RequestRunning {
		time.Sleep(time.Second)
		tr, err = c.GetTranslation(ctx, tr.ID)
	}
	if err != nil || tr.Result == nil || tr.Result.TranslatedText == "" {
		t.Fatalf("translate %+v %v", tr, err)
	}
	t.Logf("translation: %q, charged %s", tr.Result.TranslatedText, tr.ChargedMicrocredits)
	_, err = c.Translate(ctx, cloud.TranslateOptions{Q: "x", Target: "xx-invalid", IdempotencyKey: key("tr")})
	var apiErr *cloud.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("bad target: want APIError, got %v", err)
	}
	t.Logf("bad target: %d %s", apiErr.Status, apiErr.Code)

	// WAV, then transcribe that file.
	wav, err := c.WAV.Create(ctx, cloud.WAVOptions{Text: "The quick brown fox jumps over the lazy dog.", IdempotencyKey: key("wav")})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Minute, "wav job", func() error {
		wav, err = c.WAV.Get(ctx, wav.ID)
		if err != nil {
			return err
		}
		if wav.Status == cloud.JobFailed {
			t.Fatalf("wav failed: %s", wav.Error)
		}
		if wav.Status != cloud.JobCompleted {
			return errors.New(string(wav.Status))
		}
		return nil
	})
	rc, err := c.WAV.Download(ctx, wav.ID)
	if err != nil {
		t.Fatal(err)
	}
	audio, _ := io.ReadAll(rc)
	rc.Close()
	if int64(len(audio)) != wav.Result.Bytes {
		t.Fatalf("downloaded %d bytes, result says %d", len(audio), wav.Result.Bytes)
	}
	_, err = c.WAV.Create(ctx, cloud.WAVOptions{Text: "<speak><emphasis>hi</emphasis></speak>", InputFormat: "ssml", IdempotencyKey: key("wav")})
	if !errors.As(err, &apiErr) || apiErr.Code != "backend_rejected" || apiErr.Detail["code"] == nil {
		t.Fatalf("bad SSML: want backend_rejected with detail, got %v", err)
	}
	t.Logf("bad SSML detail: %v", apiErr.Detail)

	job, err := c.Transcriptions.Create(ctx, cloud.TranscriptionOptions{File: bytes.NewReader(audio), Filename: "fox.wav", IdempotencyKey: key("tx")})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Minute, "transcription job", func() error {
		job, err = c.Transcriptions.Get(ctx, job.ID)
		if err != nil {
			return err
		}
		if job.Status == cloud.JobFailed {
			t.Fatalf("transcription failed: %s", job.Error)
		}
		if job.Status != cloud.JobCompleted {
			return errors.New(string(job.Status))
		}
		return nil
	})
	if !strings.Contains(strings.ToLower(job.Result.Text), "quick brown fox") {
		t.Fatalf("transcript %q", job.Result.Text)
	}
	t.Logf("transcript: %q, speakers %d, labels %q", job.Result.Text, len(job.Result.SpeakerSegments), job.Result.SpeakerLabels)

	// Vision and moderation on a generated frame and silence.
	framePath := ffmpeg(t, "frame.jpg", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=1", "-frames:v", "1")
	frame, _ := os.ReadFile(framePath)
	v, err := c.Vision.Snapshot(ctx, cloud.VisionOptions{Image: frame, IdempotencyKey: key("v"), Question: "What colours do you see?"})
	for err == nil && v.Status == cloud.RequestRunning {
		time.Sleep(time.Second)
		v, err = c.Vision.Get(ctx, v.ID)
	}
	if err != nil || v.Result == nil || v.Result.Answer == "" {
		t.Fatalf("vision %+v %v", v, err)
	}
	t.Logf("vision: %q", v.Result.Answer)

	// Moderation takes 16 kHz PCM16: the 48 kHz WAV from above, every third sample.
	samples := audio[44:]
	pcm := make([]byte, 0, len(samples)/3)
	for i := 0; i+1 < len(samples); i += 6 {
		pcm = append(pcm, samples[i], samples[i+1])
	}
	m, err := c.Moderation.Audio(ctx, cloud.ModerateAudioOptions{Audio: pcm, IdempotencyKey: key("ma")})
	for err == nil && m.Status == cloud.RequestRunning {
		time.Sleep(time.Second)
		m, err = c.Moderation.Get(ctx, m.ID)
	}
	if err != nil || m.Result == nil {
		t.Fatalf("moderation audio %+v %v", m, err)
	}
	mv, err := c.Moderation.VideoFrame(ctx, cloud.ModerateVideoFrameOptions{Image: frame, IdempotencyKey: key("mv")})
	for err == nil && mv.Status == cloud.RequestRunning {
		time.Sleep(time.Second)
		mv, err = c.Moderation.Get(ctx, mv.ID)
	}
	if err != nil || mv.Result == nil {
		t.Fatalf("moderation video %+v %v", mv, err)
	}
	t.Logf("moderation: audio %s, video %s", m.Result.Decision, mv.Result.Decision)

	stream, err := c.Moderation.Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	sa, err := stream.SendAudio(ctx, key("sa"), pcm, "en")
	if err != nil || sa.Result == nil {
		t.Fatalf("stream audio %+v %v", sa, err)
	}
	sv, err := stream.SendVideoFrame(ctx, key("sv"), frame)
	if err != nil || sv.Result == nil {
		t.Fatalf("stream video %+v %v", sv, err)
	}
	t.Logf("moderation stream: audio %s, video %s", sa.Result.Decision, sv.Result.Decision)
}

func TestLiveWebhook(t *testing.T) {
	c := liveClient(t)
	public := os.Getenv("WEBHOOK_PUBLIC_URL")
	if public == "" {
		t.Skip("WEBHOOK_PUBLIC_URL not set")
	}
	listen := os.Getenv("WEBHOOK_LISTEN")
	if listen == "" {
		listen = ":8099"
	}
	receiver, err := cloud.NewWebhookReceiver(cloud.WebhookOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan *cloud.WebhookEvent, 16)
	failures := make(chan error, 16)
	srv := &http.Server{Addr: listen, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ev, err := receiver.ReceiveRequest(r)
		if err != nil {
			failures <- err
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		events <- ev
	})}
	go srv.ListenAndServe()
	defer srv.Close()

	ctx := context.Background()
	roomName := key("go-hook")
	tok, err := c.CreateToken(ctx, cloud.TokenOptions{Room: roomName, Identity: "hooked", TTL: 5 * time.Minute, WebhookURLs: []string{public}})
	if err != nil {
		t.Fatal(err)
	}
	b := joinBot(t, tok)
	wait := func(name string) *cloud.WebhookEvent {
		deadline := time.After(30 * time.Second)
		for {
			select {
			case ev := <-events:
				t.Logf("webhook %s %s (participant %v, room key set %v)", ev.Event, ev.ID, ev.Participant != nil, ev.Room != nil && ev.Room.Key != "")
				if ev.Event == name {
					return ev
				}
			case err := <-failures:
				t.Fatalf("webhook rejected: %v", err)
			case <-deadline:
				t.Fatalf("no %s webhook", name)
			}
		}
	}
	joined := wait(cloud.EventParticipantJoined)
	if joined.Participant == nil || joined.Participant.Identity != "hooked" {
		t.Fatalf("joined %+v", joined)
	}
	b.room.Disconnect()
	left := wait(cloud.EventParticipantLeft)
	t.Logf("participant_left rttMs=%d", left.Participant.RTTMs)
}
