package handlers

import (
	"context"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestCanceledStreamCannotFinishSuccessfullyOrReplayPendingContent(t *testing.T) {
	for _, mode := range []string{"closed channel", "buffered channel", "pending content", "cached EOF"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			chunks := make(chan coreexecutor.StreamChunk, 1)
			pending := []coreexecutor.StreamChunk{}
			closed := false
			switch mode {
			case "buffered channel":
				chunks <- coreexecutor.StreamChunk{Payload: []byte("content")}
			case "pending content":
				pending = append(pending, coreexecutor.StreamChunk{Payload: []byte("content")})
			case "cached EOF":
				closed = true
			}
			close(chunks)
			cancel()
			chunk, ok, canceled := nextStreamChunk(ctx, &pending, &closed, chunks)
			if !canceled || ok || len(chunk.Payload) != 0 {
				t.Fatalf("canceled request result = ok:%v canceled:%v payload bytes:%d", ok, canceled, len(chunk.Payload))
			}
		})
	}
}
