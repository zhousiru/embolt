package webapi

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/zhousiru/embolt/internal/control"
)

var (
	nodeRTT     = prometheus.NewDesc("embolt_node_rtt_ms", "Typical RTT to the Emby server.", []string{"node"}, nil)
	nodeRate    = prometheus.NewDesc("embolt_node_rate_mbps", "Typical rate from the Emby server.", []string{"node"}, nil)
	nodeOpen    = prometheus.NewDesc("embolt_node_breaker_open", "1 while the node's breaker is open.", []string{"node"}, nil)
	sessSafe    = prometheus.NewDesc("embolt_session_safe_mbps", "Rate the session's media node keeps to most of the time.", []string{"session"}, nil)
	sessNeed    = prometheus.NewDesc("embolt_session_need_mbps", "Rate the session needs over the horizon at its buffer.", []string{"session"}, nil)
	sessBuffer  = prometheus.NewDesc("embolt_session_buffer_seconds", "Seconds of media in the session's read-ahead.", []string{"session"}, nil)
	descriptors = []*prometheus.Desc{nodeRTT, nodeRate, nodeOpen, sessSafe, sessNeed, sessBuffer}
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
		gauge(sessSafe, s.SafeMbps, s.Key)
		gauge(sessNeed, s.NeedMbps, s.Key)
		gauge(sessBuffer, s.BufferSeconds, s.Key)
	}
}
