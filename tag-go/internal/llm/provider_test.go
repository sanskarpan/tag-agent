package llm

import (
	"context"
	"testing"
)

func TestEchoProviderStreams(t *testing.T) {
	p := EchoProvider{}
	ch, err := p.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hello world"}}})
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var finished bool
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			text += ev.Text
		case EventFinish:
			finished = true
		}
	}
	if text != "hello world" || !finished {
		t.Errorf("echo stream: text=%q finished=%v", text, finished)
	}
}

func TestRegistry(t *testing.T) {
	if _, ok := Registry["echo"]; !ok {
		t.Error("echo provider should self-register")
	}
}
