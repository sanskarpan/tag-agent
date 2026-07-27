package mcp

import (
	"bufio"
	"errors"
	"fmt"
)

// maxFrameBytes caps a single newline-delimited JSON-RPC frame on both the
// client and the server side of the stdio transport.
//
// Without a cap, bufio.Reader.ReadBytes('\n') will happily buffer an unbounded
// newline-less stream: a hostile or merely buggy peer pushed RSS past 1 GB and
// climbing in seconds. TAG's whole purpose on the client side is consuming
// THIRD-PARTY MCP servers, so an untrusted peer must not be able to OOM the
// host. (The LLM SSE parsers already cap at 4 MiB via sc.Buffer; this transport
// simply never got the same treatment.)
//
// 8 MiB is generous for a JSON-RPC frame — a tools/list from a large server is
// kilobytes — while keeping worst-case buffering bounded.
const maxFrameBytes = 8 << 20

// errFrameTooLarge is returned when a peer's frame exceeds maxFrameBytes. It is
// deliberately fatal for the connection: the stream cannot be resynchronised,
// because we never saw the frame delimiter.
var errFrameTooLarge = errors.New("mcp: frame exceeds the size limit")

// readFrame reads one newline-terminated frame, refusing to buffer more than
// maxFrameBytes. It reads through bufio.ReadSlice so the accumulation happens
// in our slice, where it can be measured, rather than inside ReadBytes where it
// cannot. The returned frame includes the trailing newline when present.
func readFrame(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxFrameBytes {
			return nil, fmt.Errorf("%w (%d bytes; limit %d)", errFrameTooLarge, len(buf)+len(chunk), maxFrameBytes)
		}
		buf = append(buf, chunk...)
		if err == nil {
			return buf, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // no delimiter yet; keep going until the cap or a newline
		}
		return buf, err
	}
}
