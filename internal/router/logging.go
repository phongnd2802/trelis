package router

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/phongnd2802/trelis/internal/logger"
	"github.com/phongnd2802/trelis/internal/types"
)

// unmatchedRoute stands in for the pattern of a request no route matched; the
// raw path is never logged.
const unmatchedRoute = "unmatched"

// requestID gives every request a new UUID, returns it in X-Request-Id and
// puts a child logger carrying it into the context. An incoming header is
// ignored: the ID identifies this request on this server, not the caller's.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-Id", id)
		l := logger.Ctx(r.Context()).With(zap.String("request_id", id))
		next.ServeHTTP(w, r.WithContext(logger.WithContext(r.Context(), l)))
	})
}

// principalHolder lets the access log see the Principal authenticate attached
// to the request it derived: the outer middleware's own request never carries
// it. authenticate records into the holder the access log put in ctx.
type principalHolder struct {
	mu sync.Mutex
	p  types.Principal
	ok bool
}

type principalHolderKey struct{}

func withPrincipalHolder(ctx context.Context, h *principalHolder) context.Context {
	return context.WithValue(ctx, principalHolderKey{}, h)
}

// recordPrincipal notes the Principal a request was authenticated as, when the
// access log put a holder in ctx.
func recordPrincipal(ctx context.Context, p types.Principal) {
	h, ok := ctx.Value(principalHolderKey{}).(*principalHolder)
	if !ok {
		return
	}
	h.mu.Lock()
	h.p, h.ok = p, true
	h.mu.Unlock()
}

func (h *principalHolder) principal() (types.Principal, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.p, h.ok
}

// accessLog writes one line per request once its handler returns: method,
// route pattern, status, duration, bytes, request_id (already on the ctx
// logger) and principal_id when authenticated. Probes are DEBUG, 5xx ERROR,
// 4xx WARN and everything else INFO.
func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		holder := &principalHolder{}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r.WithContext(withPrincipalHolder(r.Context(), holder)))

		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		route := routePattern(r)
		fields := []zap.Field{
			zap.String("method", r.Method),
			zap.String("route", route),
			zap.Int("status", status),
			zap.Int64("duration_ms", time.Since(start).Milliseconds()),
			zap.Int("bytes", ww.BytesWritten()),
		}
		if p, ok := holder.principal(); ok {
			fields = append(fields, zap.Stringer("principal_id", p.ID))
		}

		l := logger.Ctx(r.Context())
		switch {
		case route == "/healthz" || route == "/readyz":
			l.Debug("http request", fields...)
		case status >= http.StatusInternalServerError:
			l.Error("http request", fields...)
		case status >= http.StatusBadRequest:
			l.Warn("http request", fields...)
		default:
			l.Info("http request", fields...)
		}
	})
}

// routePattern is the chi pattern, never the path a caller typed.
func routePattern(r *http.Request) string {
	rc := chi.RouteContext(r.Context())
	if rc == nil {
		return unmatchedRoute
	}
	route := rc.RoutePattern()
	if route == "" || route == "/*" {
		return unmatchedRoute
	}
	return route
}

// recoverer replaces chi's middleware.Recoverer: a panic is logged with its
// stack on the request's logger and answered with the router's JSON error, so
// the access log above it still sees a 500. The stack comes from the logger's
// AddStacktrace(ErrorLevel), captured while the panic is still unwinding here.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			logger.Ctx(r.Context()).Error("panic", zap.Any("panic", v))
			writeError(w, http.StatusInternalServerError, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}
