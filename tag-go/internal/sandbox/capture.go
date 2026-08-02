package sandbox

import "fmt"

// MaxCaptureBytes is the ceiling on how much stdout (and, separately, stderr)
// either backend keeps from a sandboxed command.
//
// Both backends buffered the whole stream in memory with no limit. The command
// string is attacker-influenced in every realistic use of this package (that is
// the entire point of a sandbox), so `cat /dev/zero` or `yes` inside the
// sandbox exhausted the memory of the TAG process OUTSIDE it — the confinement
// held, and the supervisor died anyway. Docker's own --memory limit does not
// help: it bounds the container, not the pipe reader on this side.
const MaxCaptureBytes = 4 * 1024 * 1024

// capBuffer keeps at most Max bytes of what is written to it and counts the
// rest. It never reports a short write: exec's copier treats that as an error
// and tears the pipe down, which would kill a command that is merely verbose.
type capBuffer struct {
	Max     int
	buf     []byte
	dropped int
}

func newCapBuffer() *capBuffer { return &capBuffer{Max: MaxCaptureBytes} }

func (b *capBuffer) Write(p []byte) (int, error) {
	kept := 0
	if room := b.Max - len(b.buf); room > 0 {
		kept = min(room, len(p))
		b.buf = append(b.buf, p[:kept]...)
	}
	b.dropped += len(p) - kept
	return len(p), nil
}

// String renders what was kept, with an explicit notice when anything was not.
// Silently returning a shortened stream would make the Result a lie.
func (b *capBuffer) String() string {
	s := string(b.buf)
	if b.dropped > 0 {
		s += fmt.Sprintf("\n[sandbox: output truncated — %d further bytes were discarded (limit %d bytes)]\n",
			b.dropped, b.Max)
	}
	return s
}
