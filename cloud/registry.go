package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"filippo.io/edwards25519"
	"github.com/mr-tron/base58"
)

const (
	defaultRPCURL            = "https://api.mainnet-beta.solana.com"
	defaultContractAddress   = "E2FcHsC9STeB6FEtxBKGAwMTX7cbfYMyjSHKs4QbBAmh"
	defaultRegistryAuthority = "6KVRs6Yr2oYzddepFdtWrFmVq8sgELcXzbUy7apwuQX4"

	nodeRegistryName   = "dtel-nodes"
	nodeEntryHeader    = 8 // account discriminator
	nodeEntrySize      = 8 + 32 + 32 + 4 + 253 + 4 + 1
	registryRPCTimeout = 10 * time.Second
)

// nodeRegistry reads node entries from the on-chain registry.
type nodeRegistry struct {
	rpcURL   string
	program  []byte
	registry []byte // the node registry's address
	client   *http.Client
}

func newNodeRegistry(rpcURL, contract, authority string, client *http.Client) (*nodeRegistry, error) {
	program, err := base58Decode(contract)
	if err != nil || len(program) != 32 {
		return nil, fmt.Errorf("dtelecom cloud: invalid registry contract address %q", contract)
	}
	auth, err := base58Decode(authority)
	if err != nil || len(auth) != 32 {
		return nil, fmt.Errorf("dtelecom cloud: invalid registry authority %q", authority)
	}
	registry, err := findProgramAddress([][]byte{auth, []byte(nodeRegistryName)}, program)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &nodeRegistry{rpcURL: rpcURL, program: program, registry: registry, client: client}, nil
}

// isNode reports whether key has an entry in the node registry.
func (r *nodeRegistry) isNode(ctx context.Context, key []byte) (bool, error) {
	entry, err := findProgramAddress([][]byte{key, r.registry}, r.program)
	if err != nil {
		return false, err
	}
	data, err := r.accountData(ctx, base58.Encode(entry))
	if err != nil {
		return false, err
	}
	if len(data) != nodeEntrySize {
		return false, nil
	}
	registered := data[nodeEntryHeader+32 : nodeEntryHeader+64]
	return bytes.Equal(registered, key), nil
}

// accountData reads an account's data; nil when the account does not exist.
func (r *nodeRegistry) accountData(ctx context.Context, address string) ([]byte, error) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "getAccountInfo",
		"params": []any{address, map[string]string{"encoding": "base64", "commitment": "confirmed"}},
	})
	ctx, cancel := context.WithTimeout(ctx, registryRPCTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.rpcURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry rpc: %s", res.Status)
	}
	var out struct {
		Result *struct {
			Value *struct {
				Data []string `json:"data"`
			} `json:"value"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("registry rpc: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("registry rpc: %s", out.Error.Message)
	}
	if out.Result == nil || out.Result.Value == nil || len(out.Result.Value.Data) == 0 {
		return nil, nil
	}
	return base64.StdEncoding.DecodeString(out.Result.Value.Data[0])
}

// findProgramAddress derives a Solana program address: the first bump, from 255 down, whose
// hash is not a point on the ed25519 curve.
func findProgramAddress(seeds [][]byte, program []byte) ([]byte, error) {
	for bump := 255; bump >= 0; bump-- {
		h := sha256.New()
		for _, s := range seeds {
			h.Write(s)
		}
		h.Write([]byte{byte(bump)})
		h.Write(program)
		h.Write([]byte("ProgramDerivedAddress"))
		sum := h.Sum(nil)
		if _, err := new(edwards25519.Point).SetBytes(sum); err != nil {
			return sum, nil
		}
	}
	return nil, errors.New("dtelecom cloud: no program address")
}

func base58Decode(s string) ([]byte, error) {
	return base58.Decode(s)
}
