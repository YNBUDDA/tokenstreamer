package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

var logger = log.New(os.Stdout, "[TOKEN-MONITOR] ", log.Ldate|log.Ltime)

func main() {
	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	monitor, err := NewTokenMonitor(
		"wss://api.devnet.solana.com",
		"https://api.devnet.solana.com",
	)
	if err != nil {
		logger.Fatalf("Failed to create monitor: %v", err)
	}

	if err := monitor.StartMonitoring(ctx); err != nil {
		logger.Fatalf("Failed to start monitoring: %v", err)
	}

	logger.Println("Monitor running. Press Ctrl+C to stop...")

	// Handle graceful shutdown
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	logger.Println("Shutting down...")
	monitor.Stop()
}
