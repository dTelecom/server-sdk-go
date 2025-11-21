package main

import (
	"log"
	"os"

	lksdk "github.com/dtelecom/server-sdk-go"
)

func main() {
	contractAddress := os.Getenv("CONTRACT_ADDRESS")
	solanaHostHTTP := os.Getenv("SOLANA_HOST_HTTP")
	registryAuthority := os.Getenv("REGISTRY_AUTHORITY")
	selfIP := os.Getenv("SELF_IP")
	apiKey := os.Getenv("API_KEY")

	nodeProvider := lksdk.NewNodeProvider(contractAddress, solanaHostHTTP, registryAuthority, selfIP)

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