// Derived from WeKnora (https://github.com/Tencent/WeKnora), internal/models/api/transport.go
// and internal/models/api/response_limit.go.
// Copyright (C) 2026 Tencent. Licensed under the MIT License; see NOTICE.

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Fallback timeouts for LLM calls, applied only when the caller's ctx has no
// deadline, so a hung request cannot pin a worker forever.
const (
	DefaultChatTimeout   = 300 * time.Second
	DefaultStreamTimeout = 600 * time.Second
)

// WithLLMTimeout attaches d only when ctx has no deadline; a deadline the
// caller set, shorter or longer, is respected as-is.
func WithLLMTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// HTTPClient is the shared client every protocol uses. It has connection-level
// timeouts only; the per-request timeout is the context deadline (see
// WithLLMTimeout), so streaming calls are not cut off by http.Client.Timeout.
var HTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 5,
	},
}

// MaxResponseBytes bounds a single non-streaming provider response body.
const MaxResponseBytes = 64 << 20

// ErrResponseTooLarge marks a body over the cap. It is deterministic: the same
// request returns the same oversized body, so a retry cannot help.
var ErrResponseTooLarge = errors.New("response exceeds maximum size")

// ReadResponseCapped reads a non-streaming response body, refusing anything
// larger than limit instead of buffering it, because a truncated body would
// otherwise be decoded as if it were the whole reply. A non-positive limit
// falls back to MaxResponseBytes.
func ReadResponseCapped(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = MaxResponseBytes
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w (%d bytes)", ErrResponseTooLarge, limit)
	}
	return data, nil
}
