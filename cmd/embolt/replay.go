package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"text/tabwriter"
	"time"

	"github.com/zhousiru/embolt/internal/config"
	"github.com/zhousiru/embolt/internal/control"
	"github.com/zhousiru/embolt/internal/measure"
	"github.com/zhousiru/embolt/internal/nodes"
)

// replay runs the belief model over logged samples, predicting each sample
// before learning from it. A calibrated model sees about 10% of samples fall
// under its 10% quantile, 50% under the median, and 90% under the 90% one.
// It checks single samples, and what the controller actually bets on: the
// average rate over the next horizon of playback. Try candidate half_life
// and prior_strength values against your own data.
func replay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	samples := fs.String("samples", "/data/samples", "sample log directory")
	path := fs.String("config", "/config/config.yaml", "candidate config")
	fs.Parse(args)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.DiscardHandler))
	stats := measure.NewStats(config.Static(cfg), "")
	known := map[string]*nodes.Node{}

	var all []measure.Sample
	for s, err := range measure.ReadSamples(*samples) {
		if err != nil {
			return err
		}
		all = append(all, s)
	}
	ahead := horizonAverages(all, control.Horizon)

	var rate, horizon, rtt calibration
	var probeBytes int64
	hourIPs, maxIPs, hour := map[string]bool{}, 0, time.Time{}
	for i, s := range all {
		n := known[s.Node]
		if n == nil {
			n = &nodes.Node{ID: s.Node, Name: s.Node}
			known[s.Node] = n
		}
		st := stats.StateAt(n, s.Time)
		if r := s.Mbps(); r > 0 {
			rate.add(st.Rate.Predictive().CDF(math.Log(r)))
		}
		if avg, ok := ahead[i]; ok {
			horizon.add(st.Rate.Average(avg.windows).CDF(avg.logMean))
		}
		if s.Kind == measure.KindPing && s.Err == "" && s.TTFB > 0 {
			rtt.add(st.RTT.Predictive().CDF(math.Log(float64(s.TTFB) / float64(time.Millisecond))))
		}
		if s.Kind == measure.KindSpeed {
			probeBytes += s.Bytes
			if h := s.Time.Truncate(time.Hour); !h.Equal(hour) {
				hour, hourIPs = h, map[string]bool{}
			}
			hourIPs[s.Node] = true
			maxIPs = max(maxIPs, len(hourIPs))
		}
		stats.Record(n, s)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "nodes\t%d\nspeed-test bytes\t%.1f MB\nmost exit IPs in an hour\t%d\n\n", len(known), float64(probeBytes)/1e6, maxIPs)
	fmt.Fprintln(w, "belief\tsamples\t<q10\t<q50\t<q90")
	rate.print(w, "rate, one sample")
	horizon.print(w, fmt.Sprintf("rate, %s average", control.Horizon))
	rtt.print(w, "rtt")
	return w.Flush()
}

type average struct{ windows, logMean float64 }

// horizonAverages finds, for each playback sample, the mean log-rate of the
// same node's playback samples over the following horizon, where that span
// was streamed nearly throughout (no gap over 3 windows).
func horizonAverages(all []measure.Sample, h time.Duration) map[int]average {
	byNode := map[string][]int{}
	for i, s := range all {
		if s.Kind == measure.KindPassive && s.Mbps() > 0 {
			byNode[s.Node] = append(byNode[s.Node], i)
		}
	}
	out := map[int]average{}
	for _, idx := range byNode {
		for j, i := range idx {
			start, sum, k := all[i].Time, 0.0, 0
			prev := start
			for _, next := range idx[j+1:] {
				t := all[next].Time
				if t.Sub(prev) > 3*measure.Window || t.Sub(start) > h {
					break
				}
				sum += math.Log(all[next].Mbps())
				prev, k = t, k+1
			}
			if prev.Sub(start) >= h-2*measure.Window {
				out[i] = average{float64(k), sum / float64(k)}
			}
		}
	}
	return out
}

// calibration counts where samples fell in the predictive distribution.
type calibration struct{ n, q10, q50, q90 int }

func (c *calibration) add(u float64) {
	c.n++
	for _, b := range []struct {
		p   float64
		cnt *int
	}{{0.1, &c.q10}, {0.5, &c.q50}, {0.9, &c.q90}} {
		if u < b.p {
			*b.cnt++
		}
	}
}

func (c calibration) print(w io.Writer, name string) {
	pct := func(k int) string {
		if c.n == 0 {
			return "-"
		}
		return fmt.Sprintf("%.1f%%", 100*float64(k)/float64(c.n))
	}
	fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", name, c.n, pct(c.q10), pct(c.q50), pct(c.q90))
}
