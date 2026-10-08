package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestImageBudgetSharedAcrossNestedConcurrentAttempts(t *testing.T) {
	ctx := WithImageGenerationBudget(context.Background())
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ConsumeImageGeneration(WithImageGenerationBudget(ctx)) == nil {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 4 || ImageGenerationAttempts(ctx) != 4 {
		t.Fatalf("accepted=%d attempts=%d", accepted.Load(), ImageGenerationAttempts(ctx))
	}
	if err := ConsumeImageGeneration(ctx); err == nil {
		t.Fatal("budget reset")
	}
}

func TestImageBudgetCancellationConsumesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(WithImageGenerationBudget(context.Background()))
	cancel()
	if ConsumeImageGeneration(ctx) != context.Canceled || ImageGenerationAttempts(ctx) != 0 {
		t.Fatal("cancellation must prevent transport")
	}
}
