// basic_client connects to seatalk open platform websocket and prints every received envelope.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	seatalkoapisdk "git.garena.com/seatalk/seatalk-oapi-sdk-go"
)

func main() {
	wsURL := flag.String("url", seatalkoapisdk.DefaultWebSocketURL, "full WebSocket URL")
	appID := flag.String("app-id", "", "developer bot app_id (required)")
	appSecret := flag.String("app-secret", "", "developer bot app_secret (required)")
	flag.Parse()

	if *appID == "" || *appSecret == "" {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "app-id and app-secret are required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := seatalkoapisdk.NewClient(
		*appID,
		*appSecret,
		seatalkoapisdk.WithWebSocketURL(*wsURL),
		seatalkoapisdk.WithEventDispatcher(seatalkoapisdk.NewEventDispatcher()),
		seatalkoapisdk.WithLogger(log.New(os.Stdout, "seatalk_oapi_sdk_go ", log.LstdFlags)),
	)
	defer client.Close()

	result, err := client.Connect(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("registered ok, session token: %s\n", result.Token)

	if err := client.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	if ctx.Err() != nil {
		fmt.Println("shutting down")
		return
	}
	fmt.Println("connection closed")
}
