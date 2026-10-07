package helps

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/geminiresponse"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	log "github.com/sirupsen/logrus"
)

// DoGeminiGeneration counts actual generation calls and emits payload-free diagnostics.
func DoGeminiGeneration(ctx context.Context, client *http.Client, req *http.Request, model string) (*http.Response, error) {
	if !strings.HasPrefix(strings.ToLower(model), "gemini") {
		return client.Do(req)
	}
	attempt, err := geminiresponse.TakeAttempt(ctx)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if attempt > 0 {
			if resp.Header == nil {
				resp.Header = make(http.Header)
			}
			resp.Header.Set("X-CPA-Gemini-Attempts", strconv.Itoa(attempt))
		}
	}
	log.WithFields(log.Fields{
		"request_id": logging.GetRequestID(ctx), "native_model": model,
		"attempt": attempt, "source": "upstream", "status": status,
		"headers_ms": time.Since(start).Milliseconds(),
	}).Info("gemini_generation_attempt")
	return resp, err
}

func LogGeminiOutcome(ctx context.Context, model string, err error) {
	code := "success"
	if err != nil {
		code = "upstream_error"
		var typed *geminiresponse.Error
		if errors.As(err, &typed) {
			code = typed.Code
		} else if errors.Is(err, context.Canceled) {
			code = "client_cancelled"
		}
	}
	log.WithFields(log.Fields{
		"request_id": logging.GetRequestID(ctx), "native_model": model,
		"attempts": geminiresponse.Attempts(ctx), "outcome": code,
	}).Info("gemini_generation_outcome")
}
