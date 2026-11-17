package cloud

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/mr-tron/base58"
)

func TestFindProgramAddressMatchesSolanaGo(t *testing.T) {
	program := solana.MustPublicKeyFromBase58(defaultContractAddress)
	authority := solana.MustPublicKeyFromBase58(defaultRegistryAuthority)
	want, _, err := solana.FindProgramAddress([][]byte{authority.Bytes(), []byte(nodeRegistryName)}, program)
	if err != nil {
		t.Fatal(err)
	}
	got, err := findProgramAddress([][]byte{authority.Bytes(), []byte(nodeRegistryName)}, program.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("pda %s, want %s", base58.Encode(got), want)
	}
	node := solana.NewWallet().PublicKey()
	wantEntry, _, _ := solana.FindProgramAddress([][]byte{node.Bytes(), want.Bytes()}, program)
	gotEntry, _ := findProgramAddress([][]byte{node.Bytes(), got}, program.Bytes())
	if !bytes.Equal(gotEntry, wantEntry.Bytes()) {
		t.Fatal("entry pda differs")
	}
}

// nodeEntry builds a registry account for key.
func nodeEntry(key []byte, domain string) []byte {
	data := make([]byte, nodeEntrySize)
	copy(data[8+32:8+64], key)
	binary.LittleEndian.PutUint32(data[8+64:], uint32(len(domain)))
	copy(data[8+68:], domain)
	data[8+68+len(domain)+4] = 1
	return data
}

// fakeRPC answers getAccountInfo with accounts[address], or a null value.
func fakeRPC(t *testing.T, accounts map[string][]byte, calls *int32) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "getAccountInfo" {
			t.Errorf("method %q", req.Method)
		}
		data, ok := accounts[req.Params[0].(string)]
		if !ok {
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1},"value":null}}`)
			return
		}
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"context": map[string]any{"slot": 1},
			"value":   map[string]any{"data": []string{base64.StdEncoding.EncodeToString(data), "base64"}},
		}})
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func signWebhook(t *testing.T, priv ed25519.PrivateKey, iss string, body []byte, exp time.Time) string {
	t.Helper()
	sum := sha256.Sum256(body)
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": iss, "exp": exp.Unix(), "sha256": base64.StdEncoding.EncodeToString(sum[:])})
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	return "Bearer " + signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(signing)))
}

const webhookBody = `{"event":"participant_left","room":{"sid":"RM_1","name":"demo","key":"rk"},
"participant":{"sid":"PA_1","identity":"alice","state":"DISCONNECTED","joinedAt":"1763370000","rttMs":42},"id":"EV_1","createdAt":"1763370100"}`

func TestWebhookReceiver(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	iss := base58.Encode(pub)
	program, _ := base58.Decode(defaultContractAddress)
	authority, _ := base58.Decode(defaultRegistryAuthority)
	registry, _ := findProgramAddress([][]byte{authority, []byte(nodeRegistryName)}, program)
	entry, _ := findProgramAddress([][]byte{pub, registry}, program)
	var calls int32
	rpc := fakeRPC(t, map[string][]byte{base58.Encode(entry): nodeEntry(pub, "1.dtel.network")}, &calls)

	w, err := NewWebhookReceiver(WebhookOptions{RPCURL: rpc})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(webhookBody)
	auth := signWebhook(t, priv, iss, body, time.Now().Add(time.Minute))
	ev, err := w.Receive(context.Background(), body, auth)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != EventParticipantLeft || ev.ID != "EV_1" || ev.CreatedAt != 1763370100 || ev.Room.Key != "rk" ||
		ev.Participant.Identity != "alice" || ev.Participant.RTTMs != 42 || ev.Participant.State != "DISCONNECTED" {
		t.Fatalf("event %+v %+v", ev, ev.Participant)
	}
	// A token that expired seconds ago is within the allowed clock skew.
	if _, err := w.Receive(context.Background(), body, signWebhook(t, priv, iss, body, time.Now().Add(-5*time.Second))); err != nil {
		t.Fatalf("within clock skew: %v", err)
	}
	// The node is cached: a second delivery makes no RPC call.
	if _, err := w.Receive(context.Background(), body, auth); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("rpc calls %d", calls)
	}

	// ReceiveRequest reads the request.
	req := httptest.NewRequest(http.MethodPost, "/hook", bytes.NewReader(body))
	req.Header.Set("Authorization", auth)
	if _, err := w.ReceiveRequest(req); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"changed body": "",
		"expired":      signWebhook(t, priv, iss, body, time.Now().Add(-2*time.Minute)),
		"wrong signer": func() string {
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			return signWebhook(t, other, iss, body, time.Now().Add(time.Minute))
		}(),
		"empty header": "",
		"not a JWT":    "Bearer abc",
		"unknown node": func() string {
			p, k, _ := ed25519.GenerateKey(rand.Reader)
			return signWebhook(t, k, base58.Encode(p), body, time.Now().Add(time.Minute))
		}(),
	}
	for name, header := range cases {
		b := body
		if name == "changed body" {
			b = []byte(strings.Replace(webhookBody, "alice", "mallory", 1))
			header = auth
		}
		if _, err := w.Receive(context.Background(), b, header); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeWebhookTrackEvent(t *testing.T) {
	ev, err := DecodeWebhook([]byte(`{"event":"track_published","track":{"sid":"TR_1","type":"VIDEO","source":"CAMERA","width":1280,"height":720,"layers":[{"quality":"HIGH","width":1280,"height":720}]},"id":"EV_2","createdAt":1763370100}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Track.Type != "VIDEO" || ev.Track.Source != "CAMERA" || ev.Track.Layers[0].Quality != "HIGH" || ev.CreatedAt != 1763370100 {
		t.Fatalf("track %+v", ev.Track)
	}
}
