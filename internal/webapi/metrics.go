package webapi

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/siruzhou/embolt/internal/control"
)

var (
	nodeRTT     = prometheus.NewDesc("embolt_node_rtt_ms", "Predictive median RTT to the Emby server.", []string{"node"}, nil)
	nodeRate    = prometheus.NewDesc("embolt_node_rate_mbps", "Predictive median rate from the Emby server.", []string{"node"}, nil)
	nodeOpen    = prometheus.NewDesc("embolt_node_breaker_open", "1 while the node's breaker is open.", []string{"node"}, nil)
	sessRisk    = prometheus.NewDesc("embolt_session_stall_risk", "Predicted stall probability of the session's media node.", []string{"session"}, nil)
	sessBuffer  = prometheus.NewDesc("embolt_session_buffer_seconds", "Seconds of media in the session's read-ahead.", []string{"session"}, nil)
	descriptors = []*prometheus.Desc{nodeRTT, nodeRate, nodeOpen, sessRisk, sessBuffer}
)

// collector reads gauges from the controller at scrape time.
type collector struct{ ctrl *control.Controller }

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descriptors {
		ch <- d
	}
}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	gauge := func(d *prometheus.Desc, v float64, label string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, label)
	}
	for _, n := range c.ctrl.Nodes() {
		gauge(nodeRTT, n.RTTMs.Mean, n.Name)
		gauge(nodeRate, n.RateMbps.Mean, n.Name)
		open := 0.0
		if n.BreakerOpen {
			open = 1
		}
		gauge(nodeOpen, open, n.Name)
	}
	for _, s := range c.ctrl.Sessions() {
		gauge(sessRisk, s.StallRisk, s.Key)
		gauge(sessBuffer, s.BufferSeconds, s.Key)
	}
}
