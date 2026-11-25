package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/dtelecom/server-sdk-go/cloud"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: room-management ROOM_NAME")
	}
	client, err := cloud.New(cloud.Options{})
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	participants, err := client.RoomService().ListParticipants(ctx, os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if len(participants) == 0 {
		fmt.Println("No participants in this room.")
	}
	for _, participant := range participants {
		fmt.Println(participant.Identity)
	}
}
