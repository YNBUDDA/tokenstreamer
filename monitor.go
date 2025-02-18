package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

type TokenMonitor struct {
	wsEndpoint  string
	rpcEndpoint string
	wsClient    *ws.Client
	wsConn      *websocket.Conn
	rpcClient   *rpc.Client
	isRunning   bool
	tokenCache  map[solana.PublicKey]TokenInfo
	rateLimiter *rate.Limiter
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
		tm.wsClient.Close()
		tm.wsConn = nil
		return fmt.Errorf("gorilla websocket connection failed: %v", err)
	}
	tm.wsConn = conn

	tm.rpcClient = rpc.New(tm.rpcEndpoint)

	// Test RPC connection
	rpcCtx, rpcCancel := context.WithTimeout(ctx, rpcTimeout)
	defer rpcCancel()

	_, err = tm.rpcClient.GetHealth(rpcCtx)
	if err != nil {
		tm.wsClient.Close()
		tm.wsConn.Close()
		tm.wsConn = nil
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
		tm.wsConn = nil
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

			logger.Println("Successfully subscribed to SPL Token Program")

			// Heartbeat ticker
			ticker := time.NewTicker(pingInterval)
			defer ticker.Stop()

			var unsubscribeOnce sync.Once

			// Monitor subscription
			for tm.isRunning {
				select {
				case <-ticker.C:
					// Check if tm.wsConn is nil
					if tm.wsConn == nil {
						logger.Println("tm.wsConn is nil, skipping ping")
						tm.handleConnectionError(ctx)
						break
					}

					// Send a ping message using gorilla/websocket
					err := tm.wsConn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
					if err != nil {
						logger.Printf("Ping failed: %v", err)
						unsubscribeOnce.Do(func() {
							sub.Unsubscribe()
						})
						tm.handleConnectionError(ctx)
						break
					} else {
						logger.Println("Sent ping successfully")
					}

				default:
					logger.Println("Waiting for data from subscription...")

					// Create context with timeout for each receive operation
					receiveCtx, cancel := context.WithTimeout(ctx, wsTimeout)
					got, err := sub.Recv(receiveCtx)
					cancel()

					if err != nil {
						logger.Printf("Error receiving: %v", err)
						unsubscribeOnce.Do(func() {
							sub.Unsubscribe()
						})
						tm.handleConnectionError(ctx)
						break
					}

					// Check if data is nil
					if got == nil {
						logger.Println("Warning: received nil data from sub.Recv, skipping processing")
						continue
					}

					// Process the received data
					tm.processTokenData(ctx, got)
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

	if data.Value.Account == nil {
		logger.Println("PANIC: data.Value.Account is nil in processTokenData")
		return
	}

	if data.Value.Account.Data == nil {
		logger.Println("PANIC: data.Value.Account.Data is nil in processTokenData")
		return
	}

	accountData := data.Value.Account.Data.GetBinary()
	logger.Printf("Raw Account Data: %v", accountData)

	if len(accountData) < 165 {
		logger.Println("Not a token account (length < 165)")
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
		logger.Printf("Skipping token account: %s", mint.String()) // Log the mint
		return                                                     // Skip processing if token info cannot be retrieved
	}

	logger.Printf("Decoded Token Name: %s", tokenInfo.Name)
	logger.Printf("Decoded Token Symbol: %s", tokenInfo.Symbol)

	// Calculate token value (placeholder - requires external price feed)
	tokenValue := float64(amount) / float64(10^decimals)
	logger.Printf("Token Value: %.2f", tokenValue)

	// Apply filtering
	if tokenInfo.Name == "Unknown" || tokenInfo.Symbol == "UNKNOWN" || tokenValue == 0 {
		logger.Println("Skipping due to filtering") // Log why it's skipped
		return                                      // Skip processing if token name/symbol is unknown or token value is zero
	}

	logger.Printf("\n=== Token Account Activity ===")
	logger.Printf("Slot: %d", data.Context.Slot)
	logger.Printf("Time: %s", time.Now().UTC().Format(time.RFC3339))
	logger.Printf("Token Mint: %s", mint.String())
	logger.Printf("Token Name: %s", tokenInfo.Name)
	logger.Printf("Token Symbol: %s", tokenInfo.Symbol)
	logger.Printf("Owner: %s", owner.String())
	logger.Printf("Amount: %d", amount)
	logger.Printf("Decimals: %d", decimals)
	logger.Printf("Token Value: %.2f", tokenValue)
	logger.Printf("==========================\n")
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

func (tm *TokenMonitor) getTokenInfo(ctx context.Context, mint solana.PublicKey) (TokenInfo, error) {
	// Check if token info is in the cache
	if info, ok := tm.tokenCache[mint]; ok {
		return info, nil
	}

	// Apply rate limiting
	err := tm.rateLimiter.Wait(ctx)
	if err != nil {
		return TokenInfo{}, fmt.Errorf("rate limiter error: %w", err)
	}

	// Fetch token info from the RPC client
	tokenAccountInfo, err := tm.rpcClient.GetAccountInfo(ctx, mint)
	if err != nil {
		logger.Printf("RPC Error for mint %s: %v", mint.String(), err) // Log RPC errors
		return TokenInfo{}, fmt.Errorf("failed to get token account info: %w", err)
	}

	// Decode the token metadata
	metadata, err := tm.decodeMetadata(tokenAccountInfo.Value.Data.GetBinary(), mint) // Pass mint
	if err != nil {
		logger.Printf("Metadata Decode Error for mint %s: %v", mint.String(), err) // Log metadata errors
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
