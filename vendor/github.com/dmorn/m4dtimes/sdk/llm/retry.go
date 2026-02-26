package llm

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type RetryConfig struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	Jitter     float64
}

var DefaultRetryConfig = RetryConfig{
	MaxRetries: 3,
	BaseDelay:  time.Second,
	MaxDelay:   30 * time.Second,
	Jitter:     0.2,
}

type requestFn func() (*http.Response, error)

func doWithRetry(ctx context.Context, cfg RetryConfig, fn requestFn) (*http.Response, error) {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = DefaultRetryConfig.MaxRetries
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = DefaultRetryConfig.BaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = DefaultRetryConfig.MaxDelay
	}
	if cfg.Jitter <= 0 {
		cfg.Jitter = DefaultRetryConfig.Jitter
	}

	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		resp, err := fn()
		if err == nil {
			if !shouldRetryStatus(resp.StatusCode) || attempt == cfg.MaxRetries {
				return resp, nil
			}
			resp.Body.Close()
			lastErr = errors.New(resp.Status)
			delay := retryDelay(cfg, attempt, resp)
			if err := sleepContext(ctx, delay); err != nil {
				return nil, err
			}
			continue
		}

		if !shouldRetryError(err) || attempt == cfg.MaxRetries {
			return nil, err
		}
		lastErr = err
		delay := retryDelay(cfg, attempt, nil)
		if err := sleepContext(ctx, delay); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

func shouldRetryStatus(status int) bool {
	return status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func shouldRetryError(err error) bool {
	if err == nil {
		return false
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		if nerr.Timeout() {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "timeout") || strings.Contains(msg, "temporary")
}

func retryDelay(cfg RetryConfig, attempt int, resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
			return ra
		}
	}

	exp := float64(cfg.BaseDelay) * math.Pow(2, float64(attempt))
	d := time.Duration(exp)
	if d > cfg.MaxDelay {
		d = cfg.MaxDelay
	}
	jitter := 1 + ((rand.Float64()*2 - 1) * cfg.Jitter)
	if jitter < 0 {
		jitter = 0
	}
	return time.Duration(float64(d) * jitter)
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d > 0 {
			return d
		}
	}
	return 0
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
