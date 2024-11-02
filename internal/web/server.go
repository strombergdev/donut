package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"

	"github.com/flavioribeiro/donut/internal/entities"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func NewHTTPServer(
	c *entities.Config,
	mux *http.ServeMux,
	log *zap.SugaredLogger,
	lc fx.Lifecycle,
) *http.Server {
	log.Infow("Creating HTTP server",
		"host", c.HTTPHost,
		"port", c.HTTPPort)

	srv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", c.HTTPHost, c.HTTPPort),
		Handler: mux,
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				log.Errorw("Failed to start HTTP server", "error", err)
				return err
			}
			log.Infow("Starting HTTP server",
				"addr", srv.Addr,
				"handlers", []string{"/", "/demo/", "/doSignaling", "/whep"})

			// profiling server
			go func() {
				log.Infow("Starting profiling server", "port", c.PproffHTTPPort)
				if err := http.ListenAndServe(fmt.Sprintf(":%d", c.PproffHTTPPort), nil); err != nil {
					log.Errorw("Profiling server failed", "error", err)
				}
			}()

			// main server
			go func() {
				if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
					log.Errorw("HTTP server failed", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("Shutting down HTTP server")
			return srv.Shutdown(ctx)
		},
	})
	return srv
}
