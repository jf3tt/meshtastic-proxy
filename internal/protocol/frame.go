package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// MagicByte1 is the first byte of the Meshtastic frame header.
	MagicByte1 byte = 0x94
	// MagicByte2 is the second byte of the Meshtastic frame header.
	MagicByte2 byte = 0xC3
	// MaxFrameSize is the maximum allowed protobuf payload size.
	MaxFrameSize = 512
	// HeaderSize is the size of the frame header (magic1 + magic2 + len_hi + len_lo).
	HeaderSize = 4
)

// ReadFrame reads one Meshtastic frame from the reader.
// It scans for the magic bytes 0x94 0xC3, then reads the 2-byte big-endian
// length and the protobuf payload. Any bytes before the magic sequence are
// treated as debug console output and discarded.
func ReadFrame(r io.Reader) ([]byte, error) {
	buf := make([]byte, 1)
	for {
		// Scan for START1 (0x94)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("reading START1: %w", err)
		}
		if buf[0] != MagicByte1 {
			// Debug console output byte, skip
			continue
		}

		// Read next byte, expecting START2 (0xC3)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("reading START2: %w", err)
		}
		if buf[0] != MagicByte2 {
			// Not a valid frame header, continue scanning
			continue
		}

		// Read 2-byte big-endian length
		lenBuf := make([]byte, 2)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return nil, fmt.Errorf("reading frame length: %w", err)
		}
		length := binary.BigEndian.Uint16(lenBuf)

		if length > MaxFrameSize {
			// Corrupted frame, skip and rescan
			continue
		}

		if length == 0 {
			// Empty frame, skip
			continue
		}

		// Read the protobuf payload
		payload := make([]byte, length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, fmt.Errorf("reading frame payload (%d bytes): %w", length, err)
		}

		return payload, nil
	}
}

// WriteFrame writes a Meshtastic frame to the writer.
// It prepends the 4-byte header (magic bytes + big-endian length) to the payload.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > MaxFrameSize {
		return fmt.Errorf("payload too large: %d > %d", len(payload), MaxFrameSize)
	}

	header := [HeaderSize]byte{
		MagicByte1,
		MagicByte2,
		byte(len(payload) >> 8),
		byte(len(payload)),
	}

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("writing frame header: %w", err)
	}

	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("writing frame payload: %w", err)
	}

	return nil
}
