package mcp

import (
	"bufio"
	"bytes"
	"io"
)

// MCP's stdio transport is newline-delimited JSON: one message per line, with
// no embedded newlines. Reading it line-first rather than with a streaming JSON
// decoder matters for recovery — a decoder that hits a malformed frame cannot
// resynchronise and will retry the same bytes forever, whereas a bad line can
// simply be reported and skipped.
// FrameReader is exported so the stdio bridge can use the same framing.
type FrameReader struct {
	r *bufio.Reader
}

// NewFrameReader wraps r for newline-delimited JSON-RPC reading.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next non-empty frame, or io.EOF when the stream ends.
func (fr *FrameReader) Next() ([]byte, error) {
	for {
		line, err := fr.r.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			return trimmed, nil
		}
		if err != nil {
			return nil, err
		}
	}
}
