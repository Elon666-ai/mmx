package webrtc

import (
	"sync"

	"github.com/pion/rtp"
)

// RTPRewriter rewrites RTP packet headers to maintain continuity during track switching.
type RTPRewriter struct {
	mu            sync.Mutex
	lastSeqNum    uint16
	seqNumOffset  int32
	lastTimestamp uint32
	tsOffset      int64
	initialized   bool
}

// NewRTPRewriter creates a new RTP rewriter.
func NewRTPRewriter() *RTPRewriter {
	return &RTPRewriter{}
}

// RewritePacket rewrites RTP packet headers to maintain sequence number and timestamp continuity.
func (r *RTPRewriter) RewritePacket(pkt *rtp.Packet) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.initialized {
		// First packet, just record the values
		r.lastSeqNum = pkt.SequenceNumber
		r.lastTimestamp = pkt.Timestamp
		r.initialized = true
		return
	}

	// Rewrite Sequence Number to maintain continuity
	// Example: if lastSeqNum=100 and offset=50, then new packet with seq=10 becomes seq=110
	newSeq := uint16(int32(pkt.SequenceNumber) + r.seqNumOffset)
	pkt.SequenceNumber = newSeq
	r.lastSeqNum = newSeq

	// Rewrite Timestamp to maintain monotonic increase
	newTS := uint32(int64(pkt.Timestamp) + r.tsOffset)
	pkt.Timestamp = newTS
	r.lastTimestamp = newTS
}

// OnTrackSwitch is called when switching to a new track.
// It calculates new offsets to maintain continuity.
// nextSeq and nextTS are from the first packet of the new track.
func (r *RTPRewriter) OnTrackSwitch(nextSeq uint16, nextTS uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.initialized {
		// First track, initialize with these values
		r.lastSeqNum = nextSeq - 1
		r.lastTimestamp = nextTS
		r.seqNumOffset = 0
		r.tsOffset = 0
		r.initialized = true
		return
	}

	// Calculate sequence number offset to make the next packet sequential
	// Example: if lastSeqNum=100 and nextSeq=50, we want nextSeq to become 101
	// So offset = 101 - 50 = 51
	r.seqNumOffset = int32(r.lastSeqNum) + 1 - int32(nextSeq)

	// Calculate timestamp offset to maintain monotonic increase
	// Assume video at 90kHz clock, 30fps = 3000 ticks per frame
	// Add one frame interval to ensure timestamp increases
	const timestampIncrement = 3000 // ~33ms at 90kHz
	r.tsOffset = int64(r.lastTimestamp) + timestampIncrement - int64(nextTS)
}

// Reset resets the rewriter state.
func (r *RTPRewriter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.initialized = false
	r.seqNumOffset = 0
	r.tsOffset = 0
}

// GetLastSeqNum returns the last sequence number sent (for debugging).
func (r *RTPRewriter) GetLastSeqNum() uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSeqNum
}

// GetLastTimestamp returns the last timestamp sent (for debugging).
func (r *RTPRewriter) GetLastTimestamp() uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastTimestamp
}

// IsInitialized returns whether the rewriter has been initialized.
func (r *RTPRewriter) IsInitialized() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.initialized
}
