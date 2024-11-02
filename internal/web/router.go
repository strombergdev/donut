package web

import (
	"net/http"

	"github.com/flavioribeiro/donut/internal/web/handlers"
	"go.uber.org/zap"
)

type ErrorHTTPHandler interface {
	ServeHTTP(w http.ResponseWriter, r *http.Request) error
}

func NewServeMux(
	index *handlers.IndexHandler,
	signaling *handlers.SignalingHandler,
	whep *handlers.WHEPHandler,
	whip *handlers.WHIPHandler,
	l *zap.SugaredLogger,
) *http.ServeMux {

	mux := http.NewServeMux()

	mux.Handle("/", index)

	fs := http.FileServer(http.Dir("./static"))
	mux.Handle("/demo/", setHTTPNoCaching(http.StripPrefix("/demo/", fs)))

	mux.Handle("/doSignaling", setCors(errorHandler(l, signaling)))
	mux.Handle("/whep", setCors(errorHandler(l, whep)))
	mux.Handle("/whip", setCors(errorHandler(l, whip)))

	return mux
}

func setCors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "http://localhost:2345")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Max-Age", "3600")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func errorHandler(l *zap.SugaredLogger, next ErrorHTTPHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := next.ServeHTTP(w, r)
		if err != nil {
			l.Errorw("Handler error", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

func setHTTPNoCaching(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, max-age=0, must-revalidate, proxy-revalidate")
		next.ServeHTTP(w, r)
	})
}
