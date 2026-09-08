package httpapi

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"

	"avito-kitchen/internal/domain"

	"github.com/rs/zerolog/log"
)

type observedResponse struct {
	http.ResponseWriter
	status int
}

func (w *observedResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedResponse) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func errorType(err error) string { return fmt.Sprintf("%T", err) }

func observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := rand.Text()
		logger := log.With().Str("request_id", id).Logger()
		r = r.WithContext(logger.WithContext(r.Context()))
		w.Header().Set("X-Request-ID", id)
		response := &observedResponse{ResponseWriter: w}
		defer func() {
			if recover() != nil {
				logger.Error().Str("stack", string(debug.Stack())).Msg("handler panic")
				if response.status == 0 {
					write(response, 500, map[string]any{"error": domain.Error{Code: "internal_error", Message: "internal server error"}})
				}
			}
			status := response.status
			if status == 0 {
				status = http.StatusOK
			}
			logger.Info().Str("method", r.Method).Str("route", r.Pattern).Int("status", status).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("request")
		}()
		next.ServeHTTP(response, r)
	})
}
