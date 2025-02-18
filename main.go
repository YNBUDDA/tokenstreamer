package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sync" // Import the sync package
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/gorilla/websocket" // Import gorilla/websocket
	"golang.org/x/time/rate"
)

var logger = log.New(os.Stdout, "[TOKEN-MONITOR] ", log.Ldate|log.Ltime)

// Configuration constants
const (
	reconnectDelay    = 5 * time.Second
	wsTimeout         = 60 * time.Second
	rpcTimeout        = 10 * time.Second
	maxReconnectTries = 5
	pingInterval      = 15 * time.Second // Send ping every 30 seconds
)

type TokenMonitor struct {
	wsEndpoint  string
	rpcEndpoint string
	wsClient    *ws.Client
	wsConn      *websocket.Conn // Store the underlying WebSocket connection
	rpcClient   *rpc.Client
	isRunning   bool
	tokenCache  map[solana.PublicKey]TokenInfo
	rateLimiter *rate.Limiter
}

type TokenInfo struct {
	Name   string
	Symbol string
}

func NewTokenMonitor(wsEndpoint, rpcEndpoint string) (*TokenMonitor, error) {
	limiter := rate.NewLimiter(rate.Limit(10), 20)

	return &TokenMonitor{
		wsEndpoint:  wsEndpoint,
		rpcEndpoint: rpcEndpoint,
		isRunning:   false,
		tokenCache:  make(map[solana.PublicKey]TokenInfo),
		rateLimiter: limiter,
	}, nil
}

func (tm *TokenMonitor) connect(ctx context.Context) error {
	logger.Printf("Connecting to Solana network (WS: %s)...", tm.wsEndpoint)

	// Create a context with timeout for connection
	connectCtx, cancel := context.WithTimeout(ctx, wsTimeout)
	defer cancel()

	// Establish the initial ws.Client connection (for subscriptions)
	wsClient, err := ws.Connect(connectCtx, tm.wsEndpoint)
	if err != nil {
		return fmt.Errorf("websocket connection failed: %v", err)
	}
	tm.wsClient = wsClient

	// Now, establish a separate gorilla/websocket connection for pings
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(connectCtx, tm.wsEndpoint, nil)
	if err != nil {
		tm.wsClient.Close() // Close the ws.Client connection if gorilla connection fails
		tm.wsConn = nil     // Ensure tm.wsConn is set to nil
		return fmt.Errorf("gorilla websocket connection failed: %v", err)
	}
	tm.wsConn = conn // Store the gorilla/websocket connection

	tm.rpcClient = rpc.New(tm.rpcEndpoint)

	// Test RPC connection
	rpcCtx, rpcCancel := context.WithTimeout(ctx, rpcTimeout)
	defer rpcCancel()

	_, err = tm.rpcClient.GetHealth(rpcCtx)
	if err != nil {
		tm.wsClient.Close()
		tm.wsConn.Close() // Close the gorilla connection too
		tm.wsConn = nil   // Ensure tm.wsConn is set to nil
		return fmt.Errorf("RPC connection test failed: %v", err)
	}

	logger.Printf("Successfully connected to Solana network")
	return nil
}

func (tm *TokenMonitor) reconnect(ctx context.Context) error {
	if tm.wsClient != nil {
		tm.wsClient.Close()
	}
	if tm.wsConn != nil {
		tm.wsConn.Close()
		tm.wsConn = nil // Ensure tm.wsConn is set to nil
	}

	for i := 0; i < maxReconnectTries; i++ {
		logger.Printf("Attempting to reconnect (attempt %d/%d)...", i+1, maxReconnectTries)

		err := tm.connect(ctx)
		if err == nil {
			return nil
		}

		logger.Printf("Reconnection attempt failed: %v", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(reconnectDelay):
			continue
		}
	}

	return fmt.Errorf("failed to reconnect after %d attempts", maxReconnectTries)
}

func (tm *TokenMonitor) StartMonitoring(ctx context.Context) error {
	if tm.isRunning {
		return fmt.Errorf("monitor is already running")
	}

	err := tm.connect(ctx)
	if err != nil {
		return fmt.Errorf("initial connection failed: %v", err)
	}

	tm.isRunning = true
	programID := solana.MustPublicKeyFromBase58("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")

	go func() {
		for tm.isRunning {
			logger.Printf("Starting subscription to SPL Token Program: %s", programID.String())

			// Create subscription WITHOUT timeout context
			sub, err := tm.wsClient.ProgramSubscribe(
				programID,
				rpc.CommitmentConfirmed,
			)

			if err != nil {
				logger.Printf("Subscription failed: %v", err)
				tm.handleConnectionError(ctx)
				continue
			}

			logger.Println("Successfully subscribed to SPL Token Program") // Added logging

			// Heartbeat ticker
			ticker := time.NewTicker(pingInterval)
			defer ticker.Stop()

			var unsubscribeOnce sync.Once // Ensure Unsubscribe is only called once

			// Monitor subscription
			for tm.isRunning {
				select {
				case <-ticker.C:
					// Check if tm.wsConn is nil
					if tm.wsConn == nil {
						logger.Println("tm.wsConn is nil, skipping ping")
						tm.handleConnectionError(ctx) // Attempt to reconnect
						break                         // Break out of the inner loop
					}

					// Send a ping message using gorilla/websocket
					err := tm.wsConn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
					if err != nil {
						logger.Printf("Ping failed: %v", err)
						unsubscribeOnce.Do(func() { // Call Unsubscribe only once
							sub.Unsubscribe()
						})
						tm.handleConnectionError(ctx)
						break // Break out of the inner loop
					} else {
						logger.Println("Sent ping successfully") // Added logging
					}

				default:
					logger.Println("Waiting for data from subscription...") // Added logging

					// Create context with timeout for each receive operation
					receiveCtx, cancel := context.WithTimeout(ctx, wsTimeout)
					got, err := sub.Recv(receiveCtx)
					cancel()

					if err != nil {
						logger.Printf("Error receiving: %v", err)
						unsubscribeOnce.Do(func() { // Call Unsubscribe only once
							sub.Unsubscribe()
						})
						tm.handleConnectionError(ctx)
						break
					}

					// Check if data is nil
					if got == nil {
						logger.Println("Warning: received nil data from sub.Recv, skipping processing")
						continue // Skip processing this data
					}

					// Process the received data
					tm.processTokenData(ctx, got) // Pass the context
				}
			}
		}
	}()

	return nil
}

func (tm *TokenMonitor) handleConnectionError(ctx context.Context) {
	if !tm.isRunning {
		return
	}

	logger.Printf("Connection error detected, attempting to reconnect...")

	// Check tm.isRunning before unsubscribing (example - integrate with sync.Once)
	// if tm.isRunning {
	// 	unsubscribeOnce.Do(func() { // Call Unsubscribe only once
	// 		sub.Unsubscribe()
	// 	})
	// }

	err := tm.reconnect(ctx)
	if err != nil {
		logger.Printf("Failed to recover connection: %v", err)
		tm.isRunning = false
	}
}

func (tm *TokenMonitor) processTokenData(ctx context.Context, data *ws.ProgramResult) {
	if tm == nil {
		logger.Println("PANIC: tm is nil in processTokenData")
		return
	}

	if data == nil {
		logger.Println("PANIC: data is nil in processTokenData")
		return
	}

	// Check if data.Value.Account is nil
	if data.Value.Account == nil {
		logger.Println("PANIC: data.Value.Account is nil in processTokenData")
		return
	}

	// Check if data.Value.Account.Data is nil
	if data.Value.Account.Data == nil {
		logger.Println("PANIC: data.Value.Account.Data is nil in processTokenData")
		return
	}

	accountData := data.Value.Account.Data.GetBinary()
	if len(accountData) < 165 {
		return // Not a token account
	}

	// Parse mint address
	mint := solana.PublicKeyFromBytes(accountData[:32])
	owner := solana.PublicKeyFromBytes(accountData[32:64])
	amount := binary.LittleEndian.Uint64(accountData[64:72])
	decimals := accountData[73]

	// Get token info
	tokenInfo, err := tm.getTokenInfo(ctx, mint)
	if err != nil {
		logger.Printf("Error getting token info: %v", err)
		tokenInfo = TokenInfo{Name: "Unknown", Symbol: "UNKNOWN"} // Default values
	}

	// Calculate token value (placeholder - requires external price feed)
	tokenValue := float64(amount) / float64(10^decimals) // Basic calculation

	logger.Printf("\n=== Token Account Activity ===")
	logger.Printf("Slot: %d", data.Context.Slot)
	logger.Printf("Time: %s", time.Now().UTC().Format(time.RFC3339))
	logger.Printf("Token Mint: %s", mint.String())
	logger.Printf("Token Name: %s", tokenInfo.Name)     // Added token name
	logger.Printf("Token Symbol: %s", tokenInfo.Symbol) // Added token symbol
	logger.Printf("Owner: %s", owner.String())
	logger.Printf("Amount: %d", amount)
	logger.Printf("Decimals: %d", decimals)
	logger.Printf("Token Value: %.2f", tokenValue) // Added token value
	logger.Printf("==========================\n")
}

func (tm *TokenMonitor) getTokenInfo(ctx context.Context, mint solana.PublicKey) (TokenInfo, error) {
	// Check if token info is in the cache
	if info, ok := tm.tokenCache[mint]; ok {
		return info, nil
	}

	// Apply rate limiting
	err := tm.rateLimiter.Wait(ctx) // Wait until a token is available
	if err != nil {
		return TokenInfo{}, fmt.Errorf("rate limiter error: %w", err)
	}

	// Fetch token info from the RPC client
	tokenAccountInfo, err := tm.rpcClient.GetAccountInfo(ctx, mint)
	if err != nil {
		return TokenInfo{}, fmt.Errorf("failed to get token account info: %w", err)
	}

	// Decode the token metadata
	metadata, err := decodeMetadata(tokenAccountInfo.Value.Data.GetBinary())
	if err != nil {
		return TokenInfo{}, fmt.Errorf("failed to decode metadata: %w", err)
	}

	tokenInfo := TokenInfo{
		Name:   metadata.Name,
		Symbol: metadata.Symbol,
	}

	// Store token info in the cache
	tm.tokenCache[mint] = tokenInfo

	return tokenInfo, nil
}

// Metadata struct
type Metadata struct {
	Key                  int
	UpdateAuthority      solana.PublicKey
	Mint                 solana.PublicKey
	Name                 string
	Symbol               string
	URI                  string
	SellerFeeBasisPoints uint16
	Creators             []Creator
	Verified             bool
	Collection           Collection
	Uses                 Uses
}

// Creator struct
type Creator struct {
	Address  solana.PublicKey
	Verified bool
	Share    uint8
}

// Collection struct
type Collection struct {
	Verified bool
	Key      solana.PublicKey
}

// Uses struct
type Uses struct {
	UseMethod string
	Remaining uint64
	Total     uint64
}

// decodeMetadata decodes the metadata from the account data
func decodeMetadata(data []byte) (*Metadata, error) {
	if len(data) < 375 {
		return nil, fmt.Errorf("metadata buffer too small: %d", len(data))
	}

	key := int(data[0])
	updateAuthority := solana.PublicKeyFromBytes(data[1:33])
	mint := solana.PublicKeyFromBytes(data[33:65])

	nameLength := int(data[65])
	nameStart := 66
	nameEnd := nameStart + nameLength
	if nameEnd > len(data) {
		return nil, fmt.Errorf("invalid name length")
	}
	name := string(data[nameStart:nameEnd])

	symbolLength := int(data[nameEnd])
	symbolStart := nameEnd + 1
	symbolEnd := symbolStart + symbolLength
	if symbolEnd > len(data) {
		return nil, fmt.Errorf("invalid symbol length")
	}
	symbol := string(data[symbolStart:symbolEnd])

	uriLength := int(data[symbolEnd])
	uriStart := symbolEnd + 1
	uriEnd := uriStart + uriLength
	if uriEnd > len(data) {
		return nil, fmt.Errorf("invalid uri length")
	}
	uri := string(data[uriStart:uriEnd])

	sellerFeeBasisPoints := binary.LittleEndian.Uint16(data[uriEnd : uriEnd+2])

	// TODO: Implement creators, verified, collection, and uses decoding

	metadata := &Metadata{
		Key:                  key,
		UpdateAuthority:      updateAuthority,
		Mint:                 mint,
		Name:                 name,
		Symbol:               symbol,
		URI:                  uri,
		SellerFeeBasisPoints: sellerFeeBasisPoints,
	}

	return metadata, nil
}

func (tm *TokenMonitor) Stop() {
	tm.isRunning = false
	if tm.wsClient != nil {
		tm.wsClient.Close()
	}
	if tm.wsConn != nil {
		tm.wsConn.Close()
	}
}

func main() {
	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	monitor, err := NewTokenMonitor(
		"wss://api.devnet.solana.com",   // Use Devnet WebSocket endpoint
		"https://api.devnet.solana.com", // Use Devnet RPC endpoint
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
	//signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	logger.Println("Shutting down...")
	monitor.Stop()
}
