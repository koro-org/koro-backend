package httpx

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// Health routes live at the root, outside OpenAPI (04 §2.0); the authz
// matrix lists them under non_api_routes.
const (
	healthzPath = "/healthz"
	readyzPath  = "/readyz"
)

// readyTimeout bounds all readiness checks of one /readyz call.
const readyTimeout = 2 * time.Second

// Check is one pluggable readiness check (DB ping and migration version:
// BE-002; Redis ping: BE-006).
type Check struct {
	Name string
	Run  func(ctx context.Context) error
}

// Ready is the readiness state: a flag flipped by Serve (true while
// serving, false as soon as shutdown starts) plus the registered checks.
type Ready struct {
	up     atomic.Bool
	mu     sync.RWMutex
	checks []Check
}

// NewReady returns a not-yet-ready state with the given checks.
func NewReady(checks ...Check) *Ready { return &Ready{checks: slices.Clone(checks)} }

// Add registers a check. It is safe to call before or during serving.
func (r *Ready) Add(c Check) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, c)
}

// Set flips the readiness flag.
func (r *Ready) Set(up bool) { r.up.Store(up) }

// IsReady reports the readiness flag (checks are not run).
func (r *Ready) IsReady() bool { return r.up.Load() }

// handler answers 200 {"status":"ready"} or 503 {"status":"not_ready"}.
// The failing dependency is logged, never written to the body (04 §2.0).
func (r *Ready) handler(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !r.IsReady() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), readyTimeout)
		defer cancel()
		r.mu.RLock()
		checks := slices.Clone(r.checks)
		r.mu.RUnlock()
		for _, chk := range checks {
			if err := chk.Run(ctx); err != nil {
				log.LogAttrs(ctx, slog.LevelWarn, "readiness check failed", slog.String("check", chk.Name), slog.Any("err", err))
				c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready"})
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}

// healthz reports that the process is up; it checks no dependency (04 §2.0).
func healthz(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) }
