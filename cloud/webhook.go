package cloud

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Webhook event names delivered to the URLs in a participant token.
const (
	EventParticipantJoined = "participant_joined"
	EventParticipantLeft   = "participant_left"
	EventTrackPublished    = "track_published"
	EventTrackUnpublished  = "track_unpublished"
)

const (
	// maxWebhookBody bounds ReceiveRequest's read.
	maxWebhookBody = 1 << 20
	// webhookClockSkew is the clock difference allowed between a node and this server.
	webhookClockSkew = time.Minute
)

// WebhookEvent is a node's event about a participant or track. There are no retries: each event
// is sent once per URL, so make handlers idempotent by ID.
type WebhookEvent struct {
	// Event is one of the Event* constants.
	Event string `json:"event"`
	// ID is the same for every URL of one event.
	ID string `json:"id"`
	// CreatedAt is a Unix time in seconds.
	CreatedAt   Int64            `json:"createdAt"`
	Room        *Room            `json:"room,omitempty"`
	Participant *ParticipantInfo `json:"participant,omitempty"`
	Track       *TrackInfo       `json:"track,omitempty"`
}

// WebhookOptions configure a WebhookReceiver. Empty fields fall back to environment variables,
// then to dTelecom's mainnet registry.
type WebhookOptions struct {
	// RPCURL is a Solana RPC endpoint. Env: SOLANA_NETWORK_HOST_HTTP.
	RPCURL string
	// ContractAddress of the registry program. Env: SOLANA_CONTRACT_ADDRESS.
	ContractAddress string
	// RegistryAuthority of the node registry. Env: SOLANA_REGISTRY_AUTHORITY.
	RegistryAuthority string
	// HTTPClient calls the RPC; http.DefaultClient when nil.
	HTTPClient *http.Client
}

// WebhookReceiver verifies node webhooks. Each node signs a delivery with its own Ed25519 key;
// the receiver checks that the key belongs to a registered node, then the signature and the body
// hash. It needs outbound access to the Solana RPC once per node and caches the result. It is
// safe for concurrent use.
type WebhookReceiver struct {
	registry *nodeRegistry
	mu       sync.Mutex
	nodes    map[string]bool
}

// NewWebhookReceiver returns a receiver for the registry in opts.
func NewWebhookReceiver(opts WebhookOptions) (*WebhookReceiver, error) {
	reg, err := newNodeRegistry(
		firstNonEmpty(opts.RPCURL, os.Getenv("SOLANA_NETWORK_HOST_HTTP"), defaultRPCURL),
		firstNonEmpty(opts.ContractAddress, os.Getenv("SOLANA_CONTRACT_ADDRESS"), defaultContractAddress),
		firstNonEmpty(opts.RegistryAuthority, os.Getenv("SOLANA_REGISTRY_AUTHORITY"), defaultRegistryAuthority),
		opts.HTTPClient,
	)
	if err != nil {
		return nil, err
	}
	return &WebhookReceiver{registry: reg, nodes: map[string]bool{}}, nil
}

// ReceiveRequest verifies the webhook in r and returns its event.
func (w *WebhookReceiver) ReceiveRequest(r *http.Request) (*WebhookEvent, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		return nil, fmt.Errorf("dtelecom cloud: read webhook: %w", err)
	}
	if len(body) > maxWebhookBody {
		return nil, errors.New("dtelecom cloud: webhook body too large")
	}
	return w.Receive(r.Context(), body, r.Header.Get("Authorization"))
}

// Receive verifies a webhook: body is the raw POST body, authHeader the Authorization header.
func (w *WebhookReceiver) Receive(ctx context.Context, body []byte, authHeader string) (*WebhookEvent, error) {
	token := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if token == "" {
		return nil, errors.New("dtelecom cloud: authorization header is empty")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("dtelecom cloud: invalid webhook token")
	}
	var header struct {
		Alg string `json:"alg"`
	}
	var claims struct {
		Iss    string   `json:"iss"`
		Exp    *float64 `json:"exp"`
		Nbf    *float64 `json:"nbf"`
		SHA256 string   `json:"sha256"`
	}
	if err := decodeSegment(parts[0], &header); err != nil || header.Alg != "EdDSA" {
		return nil, errors.New("dtelecom cloud: invalid webhook token")
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return nil, errors.New("dtelecom cloud: invalid webhook token")
	}
	if claims.Iss == "" {
		return nil, errors.New("dtelecom cloud: webhook token has no iss")
	}
	key, err := base58Decode(claims.Iss)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("dtelecom cloud: webhook signed by an invalid key: %s", claims.Iss)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(ed25519.PublicKey(key), []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("dtelecom cloud: invalid webhook signature")
	}
	now := float64(time.Now().Unix())
	skew := webhookClockSkew.Seconds()
	if claims.Exp != nil && now >= *claims.Exp+skew {
		return nil, errors.New("dtelecom cloud: webhook token expired")
	}
	if claims.Nbf != nil && now < *claims.Nbf-skew {
		return nil, errors.New("dtelecom cloud: webhook token not valid yet")
	}
	sum := sha256.Sum256(body)
	if claims.SHA256 != base64.StdEncoding.EncodeToString(sum[:]) {
		return nil, errors.New("dtelecom cloud: sha256 checksum of body does not match")
	}
	if err := w.checkNode(ctx, claims.Iss, key); err != nil {
		return nil, err
	}
	return DecodeWebhook(body)
}

// DecodeWebhook decodes a webhook body without verifying it. Use Receive for anything you act on.
func DecodeWebhook(body []byte) (*WebhookEvent, error) {
	var e WebhookEvent
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("dtelecom cloud: decode webhook: %w", err)
	}
	return &e, nil
}

func (w *WebhookReceiver) checkNode(ctx context.Context, name string, key []byte) error {
	w.mu.Lock()
	known := w.nodes[name]
	w.mu.Unlock()
	if known {
		return nil
	}
	ok, err := w.registry.isNode(ctx, key)
	if err != nil {
		return fmt.Errorf("dtelecom cloud: check webhook node: %w", err)
	}
	if !ok {
		return fmt.Errorf("dtelecom cloud: webhook signed by unknown node: %s", name)
	}
	w.mu.Lock()
	w.nodes[name] = true
	w.mu.Unlock()
	return nil
}

func decodeSegment(s string, out any) error {
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}
