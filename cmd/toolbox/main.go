// Copyright 2024 SandrPod
// Toolbox - code execution service
// Runs inside sandbox containers

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sandrpod/sandrpod/pkg/toolbox"
)

var (
	port  = flag.Int("port", 8080, "Toolbox server port")
	token = flag.String("token", os.Getenv("TOOLBOX_TOKEN"), "Bearer token for authentication (empty = no auth)")
	help  = flag.Bool("help", false, "Show help")
)

func main() {
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}

	log.Printf("Starting SandrPod Toolbox v0.2.0 on port %d", *port)

	addr := fmt.Sprintf(":%d", *port)
	server := toolbox.NewServer(addr, *token)

	// Start the server
	go func() {
		if err := server.Start(); err != nil {
			log.Printf("Toolbox server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down Toolbox...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*toolbox.CleanupTimeout)
	defer cancel()
	server.Stop(ctx)
}