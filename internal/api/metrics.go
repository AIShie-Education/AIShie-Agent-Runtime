package api

import (
	"github.com/prometheus/client_golang/prometheus"
)

// metrics are what the API counts. Labels hold a route's pattern, a code or
// a reason, never an id.
type metrics struct {
	requests      *prometheus.CounterVec
	seconds       *prometheus.HistogramVec
	authFailures  *prometheus.CounterVec
	auditFailures prometheus.Counter
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aishie_api_requests_total", Help: "Requests to the runtime's API, by route and by the code answered (ok for a success).",
		}, []string{"route", "code"}),
		seconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "aishie_api_request_seconds", Help: "How long the runtime's API took to answer, by route.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"route"}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "aishie_api_auth_failures_total", Help: "Assertions the runtime's API refused, by reason.",
		}, []string{"reason"}),
		auditFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "aishie_api_audit_failures_total", Help: "Audit events the runtime's API could not record.",
		}),
	}
	reg.MustRegister(m.requests, m.seconds, m.authFailures, m.auditFailures)
	return m
}
