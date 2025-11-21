package main

import (
	"context"
	"log"
	"os"
	"time"

	lksdk "github.com/dtelecom/server-sdk-go"
)

func main() {
	contractAddress := os.Getenv("CONTRACT_ADDRESS")
	solanaHostHTTP := os.Getenv("SOLANA_HOST_HTTP")
	registryAuthority := os.Getenv("REGISTRY_AUTHORITY")
	apiKey := os.Getenv("API_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	selfIP, err := lksdk.DetectPublicIP(ctx)
	if err != nil {
		log.Printf("Failed to detect public IP: %v", err)
	}

	log.Println("selfIP: ", selfIP)

	nodeProvider, err := lksdk.NewNodeProvider(contractAddress, solanaHostHTTP, registryAuthority, &selfIP)
	if err != nil {
		log.Printf("Failed to create node provider: %v", err)
	}

	nodes, err := nodeProvider.List()
	if err != nil {
		log.Printf("Failed to list nodes: %v", err)
	}

	log.Println("nodes: ", nodes)

	node, err := nodeProvider.GetNode(apiKey)
	if err != nil {
		log.Printf("Failed to get node: %v", err)
	}

	log.Println("node: ", node)

	nodes, err = nodeProvider.List()
	if err != nil {
		log.Printf("Failed to list nodes: %v", err)
	}

	log.Println("nodes: ", nodes)
	
	nodesOrdered := nodeProvider.ListOrdered()
	if err != nil {
		log.Printf("Failed to list ordered nodes: %v", err)
	}

	log.Println("nodesOrdered: ", nodesOrdered)

	url := nodeProvider.LivekitAPIKeyToURL(apiKey)
	if err != nil {
		log.Printf("Failed to get livekit API key to URL: %v", err)
	}

	log.Println("url: ", url)

	urls := nodeProvider.GetNewLivekitURLs(selfIP)
	if err != nil {
		log.Printf("Failed to get new livekit URLs: %v", err)
	}

	log.Println("urls: ", urls)
}