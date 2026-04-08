package http

import (
	"expvar"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
)

// Deps contains all handler dependencies.
type Deps struct {
	PublicHandler   *PublicHandler
	DocumentHandler *DocumentHandler
	HealthHandler   *HealthHandler
	Logger          *zap.Logger
}

// NewRouter creates the chi router with all route groups and middleware.
func NewRouter(deps Deps) *chi.Mux {
	r := chi.NewRouter()

	r.Use(chimw.Recoverer)
	r.Use(RequestID)
	r.Use(RequestLogger(deps.Logger))

	// Health check (root level)
	r.Method("GET", "/health", deps.HealthHandler)

	// Public endpoints (Oathkeeper strips /api/private, arrives as /rest/storage/...)
	r.Route("/rest/storage", func(r chi.Router) {
		r.Use(JWTExtractor)
		r.Get("/document/{id}", deps.PublicHandler.ServeDocument)
	})

	// Internal endpoints (no auth)
	r.Route("/internal", func(r chi.Router) {
		r.Post("/document", deps.DocumentHandler.Create)
		r.Get("/document/{id}/meta", deps.DocumentHandler.GetMeta)
		r.Get("/document/{id}/content", deps.DocumentHandler.GetContent)
		r.Put("/document/{id}/content", deps.DocumentHandler.ReplaceContent)
		r.Delete("/document/{id}", deps.DocumentHandler.Delete)
		r.Patch("/document/{id}", deps.DocumentHandler.Update)

		// Debug/metrics
		r.Handle("/debug/vars", expvar.Handler())
	})

	return r
}
