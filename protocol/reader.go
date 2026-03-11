package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// maxPacketLength is the maximum allowed packet length (16 MB) to prevent
// memory exhaustion from malicious or malformed length headers.
const maxPacketLength = 16 * 1024 * 1024

// Reader is used for reading packets from an io.Reader.
type Reader struct {
	// r is the underlying io.Reader used for reading data.
	r io.Reader
	// hdr is a reusable buffer for reading the 4-byte length prefix.
	hdr [4]byte
}

// NewReader creates a new Reader with the given io.Reader.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// ReadPacket reads a packet from the underlying io.Reader.
// It first reads the length of the packet as an uint32 in big-endian order,
// then reads the actual packet data of that length.
func (r *Reader) ReadPacket() ([]byte, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		return nil, fmt.Errorf("failed to read packet length: %w", err)
	}
	length := binary.BigEndian.Uint32(r.hdr[:])

	if length > maxPacketLength {
		return nil, fmt.Errorf("packet length %d exceeds maximum allowed %d", length, maxPacketLength)
	}

	pk := make([]byte, length)
	if _, err := io.ReadFull(r.r, pk); err != nil {
		return nil, fmt.Errorf("failed to read packet data: %w", err)
	}
	return pk, nil
}
