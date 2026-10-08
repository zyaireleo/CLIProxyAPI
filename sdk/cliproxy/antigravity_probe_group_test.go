package cliproxy

import (
	"context"
	"sync"
	"testing"
)

func TestAntigravityProbeNextBatchCannotReusePriorCompletion(t *testing.T) {
	var group antigravityProbeGroup
	for i := 0; i < 1000; i++ {
		group.Add(1)
		first := group.doneChannel()
		group.Done()
		group.Add(1)
		second := group.doneChannel()
		if first == second {
			t.Fatal("new probe reused the prior batch completion")
		}
		select {
		case <-first:
		default:
			t.Fatal("prior batch waiter is blocked by a later probe")
		}
		select {
		case <-second:
			t.Fatal("new active batch is already complete")
		default:
		}
		group.Done()
		group.Wait()
	}
}

func TestAntigravityProbeConcurrentWaitAndCancellation(t *testing.T) {
	s := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var work sync.WaitGroup
	for i := 0; i < 12; i++ {
		work.Add(1)
		go func() {
			defer work.Done()
			for n := 0; n < 200; n++ {
				s.antigravityProbeWg.Add(1)
				s.waitAntigravityProbesContext(ctx)
				s.antigravityProbeWg.Done()
				s.WaitAntigravityProbes()
			}
		}()
	}
	work.Wait()
	if s.antigravityProbeWg.doneChannel() != nil {
		t.Fatal("probe completion leaked after all work drained")
	}
}
