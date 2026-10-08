package cliproxy

import "sync"

// antigravityProbeGroup gives each active batch its own completion channel.
// Config reloads can start a new batch while waiters for a drained batch resume.
type antigravityProbeGroup struct {
	mu      sync.Mutex
	pending int
	done    chan struct{}
}

func (g *antigravityProbeGroup) Add(count int) {
	if count <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == 0 {
		g.done = make(chan struct{})
	}
	g.pending += count
}

func (g *antigravityProbeGroup) Done() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == 0 {
		return
	}
	g.pending--
	if g.pending == 0 {
		close(g.done)
		g.done = nil
	}
}

func (g *antigravityProbeGroup) doneChannel() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

func (g *antigravityProbeGroup) Wait() {
	if done := g.doneChannel(); done != nil {
		<-done
	}
}
