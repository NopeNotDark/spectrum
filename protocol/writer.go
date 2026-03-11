package protocol

import (
	"encoding/binary"
	"io"
	"sync"
)

// Writer is used for writing packets to an io.Writer.
type Writer struct {
	// w is the underlying io.Writer used for writing data.
	w io.Writer
	// hdr is a reusable buffer for encoding the 4-byte length prefix.
	hdr [4]byte
	// buf is a reusable buffer that combines the length prefix and packet data
	// into a single write call, reducing syscall overhead.
	buf []byte
	mu  sync.Mutex
}

// NewWriter creates a new Writer with the given io.Writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{
		w:   w,
		buf: make([]byte, 0, 4096),
	}
}

// Write writes a packet to the underlying io.Writer.
// It prefixes the packet data with its length as an uint32 in big-endian order,
// then writes the combined data in a single call to the underlying io.Writer.
func (w *Writer) Write(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	binary.BigEndian.PutUint32(w.hdr[:], uint32(len(data)))
	w.buf = append(w.buf[:0], w.hdr[:]...)
	w.buf = append(w.buf, data...)
	_, err := w.w.Write(w.buf)
	return err
}
