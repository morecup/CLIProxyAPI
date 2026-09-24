package claudedesktop

import (
	"context"
	"testing"
	"time"
)

func TestMagicLinkBrowserSessionStreamsLatestFrameAndDispatchesPointer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatched := make(chan MagicLinkBrowserInput, 1)
	session := registerMagicLinkBrowserSession("interactive-test", cancel, func(event MagicLinkBrowserInput) error {
		dispatched <- event
		return nil
	})
	if session == nil {
		t.Fatal("registerMagicLinkBrowserSession() returned nil")
	}
	defer unregisterMagicLinkBrowserSession("interactive-test", session)

	session.markReady(900, 700)
	session.publishFrame("encoded-frame", 900, 700)
	events, unsubscribe, errSubscribe := session.Subscribe()
	if errSubscribe != nil {
		t.Fatalf("Subscribe() error = %v", errSubscribe)
	}
	defer unsubscribe()

	ready := <-events
	if ready.Type != "ready" || ready.Width != 900 || ready.Height != 700 {
		t.Fatalf("ready event = %#v", ready)
	}
	frame := <-events
	if frame.Type != "frame" || frame.FrameID == 0 || frame.Data != "encoded-frame" {
		t.Fatalf("frame event = %#v", frame)
	}

	pointer := MagicLinkBrowserInput{Type: "mouse", Action: "down", X: 450, Y: 350, ClickCount: 1}
	if errDispatch := session.DispatchInput(pointer); errDispatch != nil {
		t.Fatalf("DispatchInput() error = %v", errDispatch)
	}
	select {
	case got := <-dispatched:
		if got != pointer {
			t.Fatalf("dispatched event = %#v, want %#v", got, pointer)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pointer dispatch")
	}

	if errDispatch := session.DispatchInput(MagicLinkBrowserInput{Type: "keyboard", Action: "down", X: 1, Y: 1}); errDispatch == nil {
		t.Fatal("DispatchInput() accepted keyboard input")
	}
	if errDispatch := session.DispatchInput(MagicLinkBrowserInput{Type: "mouse", Action: "down", X: 901, Y: 1}); errDispatch == nil {
		t.Fatal("DispatchInput() accepted an out-of-bounds pointer")
	}
	if !CancelMagicLinkBrowserSession("interactive-test") {
		t.Fatal("CancelMagicLinkBrowserSession() = false, want true")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("browser cancellation did not cancel Chromium context")
	}
}

func TestMagicLinkBrowserMainFrameAllowed(t *testing.T) {
	tests := []struct {
		url  string
		want bool
	}{
		{url: "about:blank", want: true},
		{url: "https://claude.ai/magic-link?client=desktop", want: true},
		{url: "https://assets.claude.ai/example", want: true},
		{url: "https://example.com/", want: false},
		{url: "javascript:alert(1)", want: false},
	}
	for _, test := range tests {
		if got := magicLinkBrowserMainFrameAllowed(test.url); got != test.want {
			t.Errorf("magicLinkBrowserMainFrameAllowed(%q) = %t, want %t", test.url, got, test.want)
		}
	}
}
