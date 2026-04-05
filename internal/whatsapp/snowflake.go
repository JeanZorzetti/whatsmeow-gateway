package whatsapp

import (
	"sync"
)

const (
	// Custom epoch: 2024-01-01T00:00:00Z in milliseconds
	snowflakeEpoch = int64(1704067200000)
	nodeBits       = 10
	seqBits        = 12
	nodeShift      = seqBits
	tsShift        = nodeBits + seqBits
	seqMask        = (1 << seqBits) - 1
)

// SnowflakeGen generates time-sortable 64-bit IDs from WhatsApp message timestamps.
// Structure: [41 bits timestamp (ms)] [10 bits node] [12 bits sequence]
type SnowflakeGen struct {
	mu       sync.Mutex
	nodeID   int64
	lastTS   int64
	sequence int64
}

// NewSnowflakeGen creates a generator with the given node ID (0-1023).
func NewSnowflakeGen(nodeID int64) *SnowflakeGen {
	return &SnowflakeGen{nodeID: nodeID & ((1 << nodeBits) - 1)}
}

// Generate creates a Snowflake ID from a WhatsApp messageTimestamp (milliseconds).
func (s *SnowflakeGen) Generate(messageTimestampMs int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	ts := messageTimestampMs - snowflakeEpoch
	if ts < 0 {
		ts = 0
	}

	if ts == s.lastTS {
		s.sequence = (s.sequence + 1) & int64(seqMask)
		if s.sequence == 0 {
			ts++ // sequence overflow: increment timestamp to avoid collision
		}
	} else {
		s.sequence = 0
	}
	s.lastTS = ts

	return (ts << tsShift) | (s.nodeID << nodeShift) | s.sequence
}

// GenerateFromUnixSeconds creates a Snowflake ID from a Unix timestamp in seconds.
func (s *SnowflakeGen) GenerateFromUnixSeconds(unixSeconds uint64) int64 {
	return s.Generate(int64(unixSeconds) * 1000)
}
