package geminiresponse

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
)

type budgetKey struct{}
type Budget struct {
	limit   int32
	spent   atomic.Int32
	mu      sync.Mutex
	failure error
}

func WithBudget(ctx context.Context, limit int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 || GetBudget(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, budgetKey{}, &Budget{limit: int32(limit)})
}
func GetBudget(ctx context.Context) *Budget {
	if ctx == nil {
		return nil
	}
	b, _ := ctx.Value(budgetKey{}).(*Budget)
	return b
}
func Exhausted(ctx context.Context) bool {
	b := GetBudget(ctx)
	return b != nil && b.spent.Load() >= b.limit
}
func Attempts(ctx context.Context) int {
	if b := GetBudget(ctx); b != nil {
		return int(b.spent.Load())
	}
	return 0
}
func SaveFailure(ctx context.Context, err error) {
	if b := GetBudget(ctx); b != nil && err != nil {
		b.mu.Lock()
		b.failure = err
		b.mu.Unlock()
	}
}
func LastFailure(ctx context.Context) error {
	if b := GetBudget(ctx); b != nil {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.failure != nil {
			return b.failure
		}
	}
	return &Error{Code: "upstream_attempt_limit", Message: "Gemini upstream attempt budget exhausted", Status: 502, Stop: true}
}
func TakeAttempt(ctx context.Context) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b := GetBudget(ctx)
	if b == nil {
		return 0, nil
	}
	for {
		n := b.spent.Load()
		if n >= b.limit {
			return int(n), LastFailure(ctx)
		}
		if b.spent.CompareAndSwap(n, n+1) {
			return int(n + 1), nil
		}
	}
}
func Do(ctx context.Context, client *http.Client, request *http.Request) (*http.Response, error) {
	if _, err := TakeAttempt(ctx); err != nil {
		return nil, err
	}
	return client.Do(request)
}
