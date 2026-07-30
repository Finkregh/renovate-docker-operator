package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// httpMetrics holds the HTTP middleware metrics registered on the Recorder's
// registry.
type httpMetrics struct {
	duration *prometheus.HistogramVec
}

// initHTTP registers the http_request_duration_seconds histogram on the
// recorder's registry. Called lazily on first WrapHandler call to keep New()
// deterministic (middleware may not be needed in all contexts).
func (r *Recorder) initHTTP() {
	if r.http != nil {
		return
	}
	h := &httpMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP handler request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "code"}),
	}
	r.reg.MustRegister(h.duration)
	r.http = h
}

// WrapHandler returns an http.Handler that records request duration and status
// code into the http_request_duration_seconds histogram. The `pattern` argument
// is used as the static `route` label (never the raw URL, which would explode
// cardinality).
func (r *Recorder) WrapHandler(pattern string, h http.Handler) http.Handler {
	r.initHTTP()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, req)
		elapsed := time.Since(start)
		r.http.duration.With(prometheus.Labels{
			"route":  pattern,
			"method": req.Method,
			"code":   strconv.Itoa(sw.status),
		}).Observe(elapsed.Seconds())
	})
}

// statusWriter wraps http.ResponseWriter to capture the response status code.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.status = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.wroteHeader {
		sw.wroteHeader = true
	}
	return sw.ResponseWriter.Write(b)
}
