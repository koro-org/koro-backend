// Package httpx is the HTTP core (05 §10): gin + huma (humagin), the
// /api/v1 group, RFC 9457 error conversion, request ID, security headers,
// client IP, CSRF protection, request timeouts, health routes, the auth
// middleware skeleton and the graceful server lifecycle.
//
// All API operations are huma operations registered on App.V1 (use
// httpx/op.Koro for the x-koro-* extensions). The only raw gin routes are
// /healthz and /readyz.
package httpx

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-gonic/gin"

	"koro/internal/platform/problem"
)

// Route prefixes.
const (
	// APIPrefix is the versioned prefix of every API operation (04 §1.1).
	APIPrefix = "/api/v1"
	// WebhookPrefix is the only prefix exempt from CrossOriginProtection:
	// webhooks authenticate by signature, never by cookie (05 §10.7).
	WebhookPrefix = APIPrefix + "/webhooks/"
	// OpenAPIPath serves {OpenAPIPath}.json|.yaml when ExposeOpenAPI is set
	// (dev only; 04 §1.1).
	OpenAPIPath = APIPrefix + "/openapi"
)

// DefaultRequestTimeout applies when Deps.RequestTimeout is zero.
const DefaultRequestTimeout = 15 * time.Second

// Deps configures New.
type Deps struct {
	Log     *slog.Logger // required
	Title   string       // OpenAPI info.title; default "Koro API"
	Version string       // OpenAPI info.version; default "1.0.0"
	// ProblemTypeBase prefixes RFC 9457 "type"; default problem.DefaultTypeBase.
	ProblemTypeBase string
	// TrustedOrigins are extra origins accepted by CrossOriginProtection
	// (scheme://host[:port]); same-origin requests are always accepted.
	TrustedOrigins []string
	// TrustedProxies are the proxies whose X-Forwarded-For is honoured.
	// Empty means none: the client IP is the TCP peer.
	TrustedProxies []string
	RequestTimeout time.Duration // default DefaultRequestTimeout
	ExposeOpenAPI  bool          // serve the spec at OpenAPIPath (dev only)
	// Actors resolves credentials. nil means no resolver yet (until BE-014):
	// protected operations answer 401, public ones run anonymously.
	Actors ActorResolver
	// Ready is the readiness state; nil creates a new one.
	Ready *Ready
	// Middleware is appended after the core chain (e.g. the global IP rate
	// limit of BE-006).
	Middleware []gin.HandlerFunc
}

// App is the assembled HTTP application.
type App struct {
	Engine *gin.Engine
	API    huma.API // root API (security schemes, middleware, spec)
	V1     huma.API // /api/v1 group: register every operation here
	Ready  *Ready
}

// Handler returns the http.Handler to serve.
func (a *App) Handler() http.Handler { return a.Engine }

// New assembles the gin engine, the huma API and the /api/v1 group.
func New(d Deps) (*App, error) {
	if d.Log == nil {
		return nil, errors.New("httpx: Deps.Log is required")
	}
	base := d.ProblemTypeBase
	if base == "" {
		base = problem.DefaultTypeBase
	}
	if err := installProblemFactory(base); err != nil {
		return nil, err
	}
	if d.RequestTimeout <= 0 {
		d.RequestTimeout = DefaultRequestTimeout
	}
	if d.Ready == nil {
		d.Ready = NewReady()
	}

	cop := http.NewCrossOriginProtection()
	for _, o := range d.TrustedOrigins {
		if err := cop.AddTrustedOrigin(strings.TrimSuffix(o, "/")); err != nil {
			return nil, fmt.Errorf("httpx: trusted origin %q: %w", o, err)
		}
	}
	cop.AddInsecureBypassPattern(WebhookPrefix)

	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	if err := r.SetTrustedProxies(d.TrustedProxies); err != nil {
		return nil, fmt.Errorf("httpx: trusted proxies: %w", err)
	}
	// Middleware must be attached before any route (gin combines handlers at
	// registration time).
	r.Use(
		injectLogger(d.Log),
		requestID(),
		accessLog(d.Log),
		recoverProblem(d.Log, base),
		securityHeaders(),
		clientIP(),
		crossOrigin(cop, base),
		requestTimeout(d.RequestTimeout),
	)
	r.Use(d.Middleware...)
	r.NoRoute(func(c *gin.Context) {
		respondProblem(c, problem.ForStatus(http.StatusNotFound, "", base))
	})
	r.NoMethod(func(c *gin.Context) {
		respondProblem(c, problem.ForStatus(http.StatusMethodNotAllowed, "", base))
	})
	r.GET(healthzPath, healthz)
	r.GET(readyzPath, d.Ready.handler(d.Log))

	title, version := d.Title, d.Version
	if title == "" {
		title = "Koro API"
	}
	if version == "" {
		version = "1.0.0"
	}
	cfg := huma.DefaultConfig(title, version)
	cfg.CreateHooks = nil // no $schema member, no Link header
	cfg.SchemasPath = ""
	cfg.DocsPath = "" // no docs UI; the spec file is published instead
	cfg.OpenAPIPath = ""
	if d.ExposeOpenAPI {
		cfg.OpenAPIPath = OpenAPIPath
	}
	cfg.Info.Description = "Koro backend API. Errors are RFC 9457 application/problem+json with a stable `code`."
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		SchemeSessionCookie: {Type: "apiKey", In: "cookie", Name: SessionCookieName},
		SchemeBearer:        {Type: "http", Scheme: "bearer", BearerFormat: "opaque"},
	}

	api := humagin.New(r, cfg)
	// The Problem schema and a reusable "Problem" response are always part
	// of the spec, even before the first operation references them (huma
	// prunes unreferenced schemas, the response keeps it referenced).
	ref := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[problem.Problem](), true, "Problem")
	api.OpenAPI().Components.Responses = map[string]*huma.Response{
		"Problem": {
			Description: "RFC 9457 problem details",
			Content:     map[string]*huma.MediaType{problem.ContentType: {Schema: ref}},
		},
	}
	api.UseMiddleware(authMiddleware(api, d.Actors))
	v1 := huma.NewGroup(api, APIPrefix)

	return &App{Engine: r, API: api, V1: v1, Ready: d.Ready}, nil
}
