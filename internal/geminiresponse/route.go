package geminiresponse

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

type routeContextKey struct{}
type Route struct {
	CredentialIndex, ExitFingerprint string
	RegionalIsolation                bool
}

var regionalRoutes = struct {
	sync.Mutex
	until map[string]time.Time
}{until: make(map[string]time.Time)}

// WithRoute keeps diagnostic identity separate from credentials and proxy URLs.
func WithRoute(ctx context.Context, index, proxyURL string, isolate bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(proxyURL)))[:12]
	return context.WithValue(ctx, routeContextKey{}, Route{index, fingerprint, isolate})
}

func RouteFromContext(ctx context.Context) Route {
	if ctx == nil {
		return Route{}
	}
	route, _ := ctx.Value(routeContextKey{}).(Route)
	return route
}

func RegionalRouteBlocked(ctx context.Context) bool {
	route := RouteFromContext(ctx)
	if !route.RegionalIsolation || route.CredentialIndex == "" {
		return false
	}
	key := route.CredentialIndex + ":" + route.ExitFingerprint
	regionalRoutes.Lock()
	defer regionalRoutes.Unlock()
	if until := regionalRoutes.until[key]; until.After(time.Now()) {
		return true
	}
	delete(regionalRoutes.until, key)
	return false
}

func IsolateRegionalRoute(ctx context.Context) {
	route := RouteFromContext(ctx)
	if !route.RegionalIsolation || route.CredentialIndex == "" {
		return
	}
	regionalRoutes.Lock()
	defer regionalRoutes.Unlock()
	now := time.Now()
	for key, until := range regionalRoutes.until {
		if !until.After(now) {
			delete(regionalRoutes.until, key)
		}
	}
	regionalRoutes.until[route.CredentialIndex+":"+route.ExitFingerprint] = now.Add(5 * time.Minute)
}
