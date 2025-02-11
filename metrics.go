package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// do we have a latecy that we can track?

var metricServiceHash = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "gitopper",
	Subsystem: "service",
	Name:      "info",
	Help:      "Current hash and state for this service",
}, []string{"service", "hash", "state"})
