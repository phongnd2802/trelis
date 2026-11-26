package router

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Check probes one dependency. Probe returns nil when the dependency is up
// and must give up when its ctx is done.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// Readiness is what GET /readyz reports on.
type Readiness struct {
	Checks []Check
	// Timeout bounds every probe; a probe still running when it passes is down.
	Timeout time.Duration
}

const (
	statusUp      = "up"
	statusDown    = "down"
	statusReady   = "ready"
	statusUnready = "unready"
)

// CheckStatus is one dependency's result in GET /readyz.
type CheckStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// ReadyStatus is what GET /readyz answers with: "ready" and 200 when every
// dependency is up, "unready" and 503 otherwise.
type ReadyStatus struct {
	Status string                 `json:"status"`
	Checks map[string]CheckStatus `json:"checks"`
}

// run probes every dependency concurrently and reports whether all are up.
func (r Readiness) run(ctx context.Context) (ReadyStatus, bool) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = ReadyStatus{Status: statusReady, Checks: make(map[string]CheckStatus, len(r.Checks))}
	)
	for _, c := range r.Checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := CheckStatus{Status: statusUp}
			if err := c.Probe(ctx); err != nil {
				st = CheckStatus{Status: statusDown, Error: err.Error()}
			}
			mu.Lock()
			defer mu.Unlock()
			out.Checks[c.Name] = st
			if st.Status == statusDown {
				out.Status = statusUnready
			}
		}()
	}
	wg.Wait()
	return out, out.Status == statusReady
}

// handler answers GET /readyz.
//
//	@Summary		Readiness probe
//	@Description	Probes Postgres, object storage, Neo4j and Temporal concurrently. Answers 200 when all are up, 503 when any is down.
//	@Tags			system
//	@Produce		json
//	@Success		200	{object}	ReadyStatus
//	@Failure		503	{object}	ReadyStatus
//	@Router			/readyz [get]
func (r Readiness) handler(w http.ResponseWriter, req *http.Request) {
	status, ok := r.run(req.Context())
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}
