package lksdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
)

// Node data
type Node struct {
	Domain string
	IP     string
}

type nodeMessage struct {
	Node Node
	TTL  int64
}

func (v *nodeMessage) isExpired() bool {
	return time.Now().Unix() >= v.TTL
}

var defaultFallbackURLs = []string{
	"wss://2639923154.dtel.network",
	"wss://3115567758.dtel.network",
	"wss://3630803282.dtel.network",
}
const (
	defaultClientTTL    = 600 * time.Second
	nodeRefreshInterval = 60 * time.Second
)

// NodeProvider data
type NodeProvider struct {
	ContractAddress   string
	SolanaHostHTTP    string
	RegistryAuthority string
	SelfIP            string
	lock              sync.RWMutex
	nodeValues        map[string]nodeMessage
	nodesOrdered      []RelevantsResponse
	FallbackURLs      []string
}

type NodeProviderOption struct {
	ContractAddress string
	SolanaHostHTTP string
	RegistryAuthority string
	SelfIP *string
	FallbackURLs []string
}

// NewNodeProvider data
func NewNodeProvider(options NodeProviderOption) (*NodeProvider, error) {
	if options.SelfIP == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publicIP, err := DetectPublicIP(ctx)
		if err != nil {
			log.Printf("Failed to detect public IP: %v", err)
			return nil, fmt.Errorf("failed to detect public IP: %v", err)
		}
		options.SelfIP = &publicIP
	}
	if len(options.FallbackURLs) == 0 {
		options.FallbackURLs = defaultFallbackURLs
	}

	provider := &NodeProvider{
		ContractAddress:   options.ContractAddress,
		SolanaHostHTTP:    options.SolanaHostHTTP,
		RegistryAuthority: options.RegistryAuthority,
		SelfIP:            *options.SelfIP,
		FallbackURLs:      options.FallbackURLs,
		lock:              sync.RWMutex{},
		nodeValues:        make(map[string]nodeMessage),
	}

	provider.startRefresh()
	go provider.processRefresh()

	return provider, nil
}

// List nodes
func (p *NodeProvider) List() (map[string]Node, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	nodes := make(map[string]Node)
	for k, v := range p.nodeValues {
		if !v.isExpired() {
			nodes[k] = v.Node
		}
	}

	return nodes, nil
}

// ListOrdered nodes
func (p *NodeProvider) ListOrdered() []RelevantsResponse {
	return p.nodesOrdered
}

// GetNode node
func (p *NodeProvider) GetNode(addr string) (Node, error) {
	authority, err := solana.PublicKeyFromBase58(p.RegistryAuthority)
	if err != nil {
		return Node{}, err
	}

	address, err := solana.PublicKeyFromBase58(addr)
	if err != nil {
		return Node{}, err
	}

	client, err := NewRegistryClient(p.SolanaHostHTTP, p.ContractAddress)
	if err != nil {
		return Node{}, err
	}

	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()

	entryNode, err := client.GetNodeFromRegistry(ctx, authority, "dtel-nodes", address)
	if err != nil {
		return Node{}, err
	}

	if !entryNode.Active {
		return Node{}, fmt.Errorf("inactive node: %v", entryNode)
	}

	var ipv4 net.IP
	ips, _ := net.LookupIP(entryNode.Domain)
	for _, ip := range ips {
		ipv4 = ip.To4()
		if ipv4 != nil {
			break
		}
	}

	if ipv4 == nil {
		return Node{}, fmt.Errorf("domain error: %v", entryNode.Domain)
	}

	node := Node{
		Domain: entryNode.Domain,
		IP:     ipv4.String(),
	}

	p.save(entryNode.Registred.String(), node)
	return node, nil
}

func (p *NodeProvider) refresh(ctx context.Context) error {
	authority, err := solana.PublicKeyFromBase58(p.RegistryAuthority)
	if err != nil {
		return err
	}

	client, err := NewRegistryClient(p.SolanaHostHTTP, p.ContractAddress)
	if err != nil {
		return err
	}

	defer client.Close()

	entryNodes, err := client.ListNodesInRegistry(ctx, authority, "dtel-nodes")
	if err != nil {
		return err
	}
	for _, entryNode := range entryNodes {
		if !entryNode.Active {
			continue
		}

		var ipv4 net.IP
		ips, _ := net.LookupIP(entryNode.Domain)
		for _, ip := range ips {
			ipv4 = ip.To4()
			if ipv4 != nil {
				break
			}
		}
		if ipv4 == nil {
			log.Printf("ipv4 nil: %v", fmt.Errorf("domain error: %v", entryNode.Domain))
			continue
		}

		node := Node{
			Domain: entryNode.Domain,
			IP:     ipv4.String(),
		}
		p.save(entryNode.Registred.String(), node)
	}
	return nil
}

func (p *NodeProvider) refreshOrdered() {
	nodes, err := p.List()

	if err != nil {
		log.Printf("refreshOrdered err: %v", err)
		return
	}

	for _, node := range nodes {
		res, err := p.GetRelevants(p.SelfIP, node.Domain)
		if err != nil {
			log.Printf("refreshOrdered err: %v", err)
		} else {
			if len(res) > 0 {
				p.nodesOrdered = res
				break
			}
		}
	}
}

func (p *NodeProvider) save(id string, node Node) {
	p.lock.Lock()
	defer p.lock.Unlock()
	msg := nodeMessage{
		Node: node,
		TTL:  time.Now().Add(defaultClientTTL).Unix(),
	}
	p.nodeValues[id] = msg
}

func (p *NodeProvider) startRefresh() {
	go func() {
		ticker := time.NewTicker(nodeRefreshInterval)
		for {
			<-ticker.C
			p.processRefresh()
		}
	}()
}

func (p *NodeProvider) processRefresh() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	err := p.refresh(ctx)
	if err != nil {
		log.Printf("[refresh] error %s\r\n", err)
	}
	p.refreshOrdered()
}

// RelevantRequest data
type RelevantRequest struct {
	IP string `json:"ip"`
}

// RelevantsResponse data
type RelevantsResponse struct {
	ID           string  `json:"id"`
	Participants int     `json:"participants"`
	Domain       string  `json:"domain"`
	IP           string  `json:"ip"`
	Country      string  `json:"country"`
	City         string  `json:"city"`
	Latitude     float64 `json:"latitude"`
	Longitude    float64 `json:"longitude"`
}

// GetRelevants nodes
func (p *NodeProvider) GetRelevants(ip string, domain string) ([]RelevantsResponse, error) {
	var res []RelevantsResponse

	req := &RelevantRequest{
		IP: ip,
	}

	ctxTimeout, cancel := context.WithTimeout(context.Background(), time.Second*1)
	defer cancel()

	url := "https://" + domain + "/relevants"

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctxTimeout, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	return res, err
}

func (p *NodeProvider) LivekitAPIKeyToURL(apiKey string) string {
	nodes, err := p.List()
	if err != nil {
		log.Printf("LivekitAPIKeyToURL err: %v", err)
	}

	node, ok := nodes[apiKey]
	if !ok {
		log.Printf("LivekitAPIKeyToURL err: %v", "not in map")
		node, err = p.GetNode(apiKey)
		if err != nil {
			return ""
		}
	}
	return "wss://" + node.Domain
}

// GetNewLivekitURLs urls
func (p *NodeProvider) GetNewLivekitURLs(ip string) []string {
	nodes := p.ListOrdered()

	var fallback []string

	for _, node := range nodes {
		res, err := p.GetRelevants(ip, node.Domain)
		if err != nil {
			log.Printf("GetNewLivekitURL err: %v", err)
		} else {
			for _, r := range res {
				if r.Domain != "" {
					fallback = append(fallback, "wss://"+r.Domain)
				}
			}
			if len(fallback) > 0 {
				break
			}
		}
	}

	if len(fallback) == 0 {
		i := 0
		for _, node := range nodes {
			if i > 2 {
				break
			}
			if node.Domain != "" {
				fallback = append(fallback, "wss://"+node.Domain)
				i++
			}
		}
	}

	if len(fallback) == 0 {
		fallback = append(fallback, defaultFallbackURLs...)
	}

	return fallback
}

func DetectPublicIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org?format=text", nil)
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(b)), nil
}
