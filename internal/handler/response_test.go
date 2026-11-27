package handler

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/phongnd2802/trelis/internal/logger"
)

func TestABodyThatCannotBeEncodedIsALogged500(t *testing.T) {
	core, logs := observer.New(zapcore.ErrorLevel)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := logger.WithContext(r.Context(), zap.New(core))
		writeData(ctx, w, http.StatusOK, math.NaN())
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	want := internalErrorJSON + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %s, want the generic internal-error envelope %s", rec.Body, want)
	}
	entries := logs.All()
	if len(entries) != 1 {
		t.Fatalf("logged %d entries, want the encode failure once", len(entries))
	}
	if !strings.Contains(entries[0].Message, "encode response body") {
		t.Errorf("log message = %q, want it to name the encode failure", entries[0].Message)
	}
}
