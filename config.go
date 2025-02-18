package main

import "time"

// Configuration constants
const (
	reconnectDelay    = 5 * time.Second
	wsTimeout         = 60 * time.Second
	rpcTimeout        = 10 * time.Second
	maxReconnectTries = 5
	pingInterval      = 30 * time.Second
)
