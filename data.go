package main

import (
	"encoding/binary"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

type TokenInfo struct {
	Name   string
	Symbol string
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
// Added mint as argument for debugging purposes
func (tm *TokenMonitor) decodeMetadata(data []byte, mint solana.PublicKey) (*Metadata, error) {
	if len(data) < 82 {
		return nil, fmt.Errorf("metadata buffer too small: %d", len(data))
	}

	logger.Printf("Decoding metadata for mint: %s", mint.String()) // Log the mint

	// Log the first few bytes for inspection
	logger.Printf("First 10 bytes of metadata: %v", data[:10])

	key := int(data[0])
	updateAuthority := solana.PublicKeyFromBytes(data[1:33])
	mintKey := solana.PublicKeyFromBytes(data[33:65])

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
		Mint:                 mintKey,
		Name:                 name,
		Symbol:               symbol,
		URI:                  uri,
		SellerFeeBasisPoints: sellerFeeBasisPoints,
	}

	return metadata, nil
}
