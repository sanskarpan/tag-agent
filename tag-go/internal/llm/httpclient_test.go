package llm

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestDefaultHTTPClientIsBounded guards the transport-level half of the
// silent-hang fix: every provider that doesn't inject its own client must get
// a transport whose ResponseHeaderTimeout is set, so a server that accepts the
// socket and never answers fails fast instead of blocking for the whole
// request timeout.
func TestDefaultHTTPClientIsBounded(t *testing.T) {
	c := DefaultHTTPClient()
	if c.Timeout <= 0 || c.Timeout > 15*time.Minute {
		t.Errorf("client Timeout = %v, want a positive sane bound", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout is unset: a stalled provider can hang the CLI")
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout is unset")
	}
}

// TestStreamFailsFastOnStalledServer proves the bound end-to-end against a
// listener that accepts and never writes: the adapter must return an error
// promptly rather than block.
func TestStreamFailsFastOnStalledServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()

	client := &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 500 * time.Millisecond}}
	p := LocalProvider{BaseURL: "http://" + ln.Addr().String() + "/v1", HTTPClient: client}
	done := make(chan error, 1)
	go func() {
		_, e := p.Stream(context.Background(), Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
		done <- e
	}()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("stalled server should have produced an error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stream hung on a stalled server")
	}
}
