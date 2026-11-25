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

type checkStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type readyStatus struct {
	Status string                 `json:"status"`
	Checks map[string]checkStatus `json:"checks"`
}

// run probes every dependency concurrently and reports whether all are up.
func (r Readiness) run(ctx context.Context) (readyStatus, bool) {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = readyStatus{Status: statusReady, Checks: make(map[string]checkStatus, len(r.Checks))}
	)
	for _, c := range r.Checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := checkStatus{Status: statusUp}
			if err := c.Probe(ctx); err != nil {
				st = checkStatus{Status: statusDown, Error: err.Error()}
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

func (r Readiness) handler(w http.ResponseWriter, req *http.Request) {
	status, ok := r.run(req.Context())
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}
