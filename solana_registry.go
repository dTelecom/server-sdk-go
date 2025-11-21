package lksdk

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

const (
	//NodeEntrySize discriminator + parent + registered + domain length + domain + online + active
	NodeEntrySize = 8 + 32 + 32 + 4 + 253 + 4 + 1
)

// RegistryClient represents a client for interacting with the registry program
type RegistryClient struct {
	programID solana.PublicKey
	client    *rpc.Client
}

// NodeEntry represents a node entry in the registry
type NodeEntry struct {
	Parent    solana.PublicKey
	Registred solana.PublicKey
	Domain    string
	Online    int32
	Active    bool
}

// NewRegistryClient creates a new instance of the registry client
func NewRegistryClient(rpcEndpoint string, programID string) (*RegistryClient, error) {
	client := rpc.New(rpcEndpoint)

	programPubkey, err := solana.PublicKeyFromBase58(programID)
	if err != nil {
		return nil, fmt.Errorf("invalid program ID: %v", err)
	}

	return &RegistryClient{
		programID: programPubkey,
		client:    client,
	}, nil
}

// Close client after use
func (c *RegistryClient) Close() {
	c.client.Close()
}

// GetNodeFromRegistry retrieves a node entry from the registry
func (c *RegistryClient) GetNodeFromRegistry(ctx context.Context, authority solana.PublicKey, registryName string, accountToCheck solana.PublicKey) (*NodeEntry, error) {
	// Find the registry PDA
	registryPDA, _, err := findRegistryPDA(c.programID, authority, registryName)
	if err != nil {
		return nil, fmt.Errorf("failed to find registry PDA: %v", err)
	}

	return getNodeEntry(ctx, c.client, c.programID, registryPDA, accountToCheck)
}

// ListNodesInRegistry retrieves all node entries in the given registry
func (c *RegistryClient) ListNodesInRegistry(ctx context.Context, authority solana.PublicKey, registryName string) ([]*NodeEntry, error) {
	// Find the registry PDA
	registryPDA, _, err := findRegistryPDA(c.programID, authority, registryName)
	if err != nil {
		return nil, fmt.Errorf("failed to find registry PDA: %v", err)
	}

	// Get all program accounts of type NodeEntry
	filters := []rpc.RPCFilter{
		{
			Memcmp: &rpc.RPCFilterMemcmp{
				Offset: 8, // Skip discriminator
				Bytes:  registryPDA.Bytes(),
			},
		},
		{
			DataSize: NodeEntrySize,
		},
	}

	accounts, err := c.client.GetProgramAccountsWithOpts(
		ctx,
		c.programID,
		&rpc.GetProgramAccountsOpts{
			Filters:    filters,
			Commitment: rpc.CommitmentFinalized,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get program accounts: %v", err)
	}

	entries := make([]*NodeEntry, 0, len(accounts))
	for _, acc := range accounts {
		data := acc.Account.Data.GetBinary()
		if len(data) != NodeEntrySize {
			continue
		}

		// Skip the 8-byte discriminator
		data = data[8:]

		// Read domain string length (4 bytes)
		domainLen := binary.LittleEndian.Uint32(data[64:68])

		entry := &NodeEntry{
			Parent:    solana.PublicKeyFromBytes(data[:32]),
			Registred: solana.PublicKeyFromBytes(data[32:64]),
			Domain:    string(data[68 : 68+domainLen]),
			Online:    int32(binary.LittleEndian.Uint32(data[68+domainLen : 72+domainLen])),
			Active:    data[72+domainLen] == 1,
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// findRegistryPDA finds the PDA for a registry with the given name
func findRegistryPDA(programID solana.PublicKey, authority solana.PublicKey, name string) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress(
		[][]byte{
			authority.Bytes(),
			[]byte(name),
		},
		programID,
	)
}

// findRegistryEntryPDA finds the PDA for a registry entry
func findRegistryEntryPDA(programID solana.PublicKey, accountToAdd solana.PublicKey, registry solana.PublicKey) (solana.PublicKey, uint8, error) {
	return solana.FindProgramAddress(
		[][]byte{
			accountToAdd.Bytes(),
			registry.Bytes(),
		},
		programID,
	)
}

// getNodeEntry retrieves a node entry account data
func getNodeEntry(
	ctx context.Context,
	client *rpc.Client,
	programID solana.PublicKey,
	registry solana.PublicKey,
	accountToCheck solana.PublicKey,
) (*NodeEntry, error) {
	entryPDA, _, err := findRegistryEntryPDA(programID, accountToCheck, registry)
	if err != nil {
		return nil, fmt.Errorf("failed to find entry PDA: %v", err)
	}

	// Get the account info
	accountInfo, err := client.GetAccountInfo(ctx, entryPDA)
	if err != nil {
		return nil, fmt.Errorf("failed to get account info: %v", err)
	}

	if accountInfo == nil || len(accountInfo.Value.Data.GetBinary()) == 0 {
		return nil, nil // Account doesn't exist
	}

	// Parse the account data
	data := accountInfo.Value.Data.GetBinary()
	if len(data) != NodeEntrySize {
		return nil, fmt.Errorf("invalid account data size: expected %d, got %d", NodeEntrySize, len(data))
	}

	// Skip the 8-byte discriminator
	data = data[8:]

	// Read domain string length (4 bytes)
	domainLen := binary.LittleEndian.Uint32(data[64:68])

	entry := &NodeEntry{
		Parent:    solana.PublicKeyFromBytes(data[:32]),
		Registred: solana.PublicKeyFromBytes(data[32:64]),
		Domain:    string(data[68 : 68+domainLen]),
		Online:    int32(binary.LittleEndian.Uint32(data[68+domainLen : 72+domainLen])),
		Active:    data[72+domainLen] == 1,
	}

	return entry, nil
}

