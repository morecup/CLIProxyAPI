package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestStreamAttemptCompletionWaitsForEOF(t *testing.T) {
	ctx := cliproxyexecutor.WithUpstreamAttemptChain(context.Background(), time.Now())
	release := cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
	finalized := make(chan struct{})
	cliproxyexecutor.RegisterUpstreamFailureFinalizer(ctx, "call", func() { close(finalized) })
	source := make(chan cliproxyexecutor.StreamChunk, 1)
	source <- cliproxyexecutor.StreamChunk{Payload: []byte("synthetic")}
	result := completeAttemptsAfterStream(ctx, &cliproxyexecutor.StreamResult{Chunks: source}, release)
	chunk := <-result.Chunks
	if string(chunk.Payload) != "synthetic" {
		t.Fatal("stream payload changed")
	}
	select {
	case <-finalized:
		t.Fatal("headers/first chunk terminated attempt chain")
	default:
	}
	close(source)
	for range result.Chunks {
	}
	select {
	case <-finalized:
	default:
		t.Fatal("EOF did not finalize attempt chain")
	}
}

func TestStreamAttemptCompletionOnCancelledReader(t *testing.T) {
	ctx, cancel := context.WithCancel(cliproxyexecutor.WithUpstreamAttemptChain(context.Background(), time.Now()))
	release := cliproxyexecutor.BeginUpstreamCompletionScope(ctx)
	finalized := make(chan struct{})
	cliproxyexecutor.RegisterUpstreamFailureFinalizer(ctx, "call", func() { close(finalized) })
	source := make(chan cliproxyexecutor.StreamChunk)
	result := completeAttemptsAfterStream(ctx, &cliproxyexecutor.StreamResult{Chunks: source}, release)
	cancel()
	select {
	case <-finalized:
	case <-time.After(time.Second):
		t.Fatal("cancelled reader retained attempt completion scope")
	}
	select {
	case _, open := <-result.Chunks:
		if !open {
			t.Fatal("outward stream closed before upstream accounting settled")
		}
	default:
	}
	close(source)
	for range result.Chunks {
	}
}
