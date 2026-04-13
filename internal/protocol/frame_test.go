package protocol

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestWriteFrame(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    []byte
		wantErr bool
	}{
		{
			name:    "simple payload",
			payload: []byte{0x01, 0x02, 0x03},
			want:    []byte{0x94, 0xC3, 0x00, 0x03, 0x01, 0x02, 0x03},
		},
		{
			name:    "single byte",
			payload: []byte{0xFF},
			want:    []byte{0x94, 0xC3, 0x00, 0x01, 0xFF},
		},
		{
			name:    "max size payload",
			payload: make([]byte, MaxFrameSize),
			want:    nil, // too long to check exact bytes
		},
		{
			name:    "over max size",
			payload: make([]byte, MaxFrameSize+1),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := WriteFrame(&buf, tt.payload)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.want != nil {
				if !bytes.Equal(buf.Bytes(), tt.want) {
					t.Fatalf("got %x, want %x", buf.Bytes(), tt.want)
				}
			}

			// Verify header
			data := buf.Bytes()
			if data[0] != MagicByte1 || data[1] != MagicByte2 {
				t.Fatalf("invalid magic bytes: %x %x", data[0], data[1])
			}
			length := int(data[2])<<8 | int(data[3])
			if length != len(tt.payload) {
				t.Fatalf("length mismatch: header says %d, payload is %d", length, len(tt.payload))
			}
		})
	}
}

func TestReadFrame(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		want    []byte
		wantErr bool
	}{
		{
			name:  "simple frame",
			input: []byte{0x94, 0xC3, 0x00, 0x03, 0xAA, 0xBB, 0xCC},
			want:  []byte{0xAA, 0xBB, 0xCC},
		},
		{
			name:  "frame with leading debug bytes",
			input: []byte{'H', 'e', 'l', 'l', 'o', 0x94, 0xC3, 0x00, 0x02, 0x01, 0x02},
			want:  []byte{0x01, 0x02},
		},
		{
			name:  "false start - 0x94 not followed by 0xC3",
			input: []byte{0x94, 0x00, 0x94, 0xC3, 0x00, 0x01, 0xFF},
			want:  []byte{0xFF},
		},
		{
			name:    "truncated header",
			input:   []byte{0x94, 0xC3, 0x00},
			wantErr: true,
		},
		{
			name:    "truncated payload",
			input:   []byte{0x94, 0xC3, 0x00, 0x05, 0x01, 0x02},
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   []byte{},
			wantErr: true,
		},
		{
			name: "oversized frame skipped, then valid frame",
			input: func() []byte {
				// First frame: length = 513 (> MaxFrameSize)
				bad := []byte{0x94, 0xC3, 0x02, 0x01}
				// Payload of 513 bytes that is discarded
				bad = append(bad, make([]byte, 513)...)
				// Then a valid frame
				good := []byte{0x94, 0xC3, 0x00, 0x01, 0x42}
				return append(bad, good...)
			}(),
			want: []byte{0x42},
		},
		{
			name: "oversized frame with magic bytes inside payload",
			input: func() []byte {
				// Oversized frame: length = 514
				header := []byte{0x94, 0xC3, 0x02, 0x02}
				payload := make([]byte, 514)
				// Plant fake magic bytes inside the payload — they must be skipped.
				payload[10] = 0x94
				payload[11] = 0xC3
				payload[12] = 0x00
				payload[13] = 0x01
				payload[14] = 0xFF // fake payload byte
				header = append(header, payload...)
				// Real valid frame after the oversized one.
				good := []byte{0x94, 0xC3, 0x00, 0x02, 0xAB, 0xCD}
				return append(header, good...)
			}(),
			want: []byte{0xAB, 0xCD},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := bytes.NewReader(tt.input)
			got, err := ReadFrame(r)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !bytes.Equal(got, tt.want) {
				t.Fatalf("got %x, want %x", got, tt.want)
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	payloads := [][]byte{
		{0x01},
		{0x01, 0x02, 0x03, 0x04, 0x05},
		bytes.Repeat([]byte{0xAB}, 100),
		bytes.Repeat([]byte{0xCD}, MaxFrameSize),
	}

	for i, payload := range payloads {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, payload); err != nil {
			t.Fatalf("payload %d: write error: %v", i, err)
		}

		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("payload %d: read error: %v", i, err)
		}

		if !bytes.Equal(got, payload) {
			t.Fatalf("payload %d: round-trip mismatch: got %d bytes, want %d bytes", i, len(got), len(payload))
		}
	}
}

func TestMultipleFrames(t *testing.T) {
	var buf bytes.Buffer

	payloads := [][]byte{
		{0x01, 0x02},
		{0x03, 0x04, 0x05},
		{0x06},
	}

	for _, p := range payloads {
		if err := WriteFrame(&buf, p); err != nil {
			t.Fatalf("write error: %v", err)
		}
	}

	for i, want := range payloads {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: read error: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d: got %x, want %x", i, got, want)
		}
	}

	// Should get EOF for next read
	_, err := ReadFrame(&buf)
	if err == nil {
		t.Fatal("expected error after all frames read")
	}
	if !errors.Is(err, io.EOF) && !bytes.Contains([]byte(err.Error()), []byte("EOF")) {
		t.Fatalf("expected EOF-related error, got: %v", err)
	}
}
