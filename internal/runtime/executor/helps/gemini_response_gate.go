package helps

import (
	"bytes"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
)

// GeminiResponseGate buffers metadata until generation starts, without a network deadline.
type GeminiResponseGate struct {
	Summary                       geminiresponse.Summary
	native, requireImage, started bool
	pending                       [][]byte
	bytes                         int
}

func NewGeminiResponseGate(native, requireImage bool) *GeminiResponseGate {
	return &GeminiResponseGate{native: native, requireImage: requireImage}
}
func (g *GeminiResponseGate) Observe(payload []byte) ([][]byte, error) {
	g.Summary.Observe(payload)
	if g.Summary.ErrorStatus != 0 || (g.Summary.Block != "" && !g.native) || (g.Summary.Finish == "MAX_TOKENS" && !g.Summary.Answer && !g.native) {
		return nil, g.Summary.Failure(g.requireImage, g.native)
	}
	if g.started || g.Summary.Answer || g.Summary.Thought || (g.native && g.Summary.Block != "") {
		g.started = true
		chunks := append(g.pending, bytes.Clone(payload))
		g.pending = nil
		g.bytes = 0
		return chunks, nil
	}
	g.bytes += len(payload)
	if g.bytes > 1<<20 {
		return nil, &geminiresponse.Error{Code: "upstream_empty_response", Message: "Gemini metadata exceeded the response bootstrap limit", Status: 502}
	}
	g.pending = append(g.pending, bytes.Clone(payload))
	return nil, nil
}
func (g *GeminiResponseGate) Finish() ([][]byte, error) {
	if err := g.Summary.Failure(g.requireImage, g.native); err != nil {
		return nil, err
	}
	chunks := g.pending
	g.pending = nil
	return chunks, nil
}
