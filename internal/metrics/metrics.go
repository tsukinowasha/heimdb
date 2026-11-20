// Package metrics は、指標を Prometheus のテキスト形式（0.0.4）で書き出す（docs/plan.md の段階 73）。
package metrics

import (
	"bytes"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Writer は、指標を書き出す。同じ名前の値（ラベルだけが違う）は続けて書く（説明と型は最初の 1 回だけ出す）。
type Writer struct {
	buf  bytes.Buffer
	last string
}

// Gauge は、今の値 v を書く。labels は名前と値を交互に並べる。
func (w *Writer) Gauge(name, help string, v float64, labels ...string) {
	w.sample(name, "gauge", help, name, v, labels)
}

// Counter は、起動から増え続ける値 v を書く。
func (w *Writer) Counter(name, help string, v float64, labels ...string) {
	w.sample(name, "counter", help, name, v, labels)
}

func (w *Writer) header(name, typ, help string) {
	if name == w.last {
		return
	}
	w.last = name
	w.buf.WriteString("# HELP " + name + " " + strings.ReplaceAll(strings.ReplaceAll(help, `\`, `\\`), "\n", `\n`) + "\n")
	w.buf.WriteString("# TYPE " + name + " " + typ + "\n")
}

func (w *Writer) sample(family, typ, help, name string, v float64, labels []string) {
	w.header(family, typ, help)
	w.buf.WriteString(name)
	if len(labels) > 0 {
		w.buf.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				w.buf.WriteByte(',')
			}
			w.buf.WriteString(labels[i] + `="` + escapeLabel(labels[i+1]) + `"`)
		}
		w.buf.WriteByte('}')
	}
	w.buf.WriteByte(' ')
	w.buf.WriteString(formatValue(v))
	w.buf.WriteByte('\n')
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Histogram は、時間の分布。境目は秒で、増える順に並べる。
type Histogram struct {
	bounds []float64
	counts []atomic.Uint64 // 境目ごと（累積ではない）。最後は最大の境目を超えた分
	sum    atomic.Int64    // ナノ秒
}

// NewHistogram は、境目 bounds（秒）の Histogram を作る。
func NewHistogram(bounds ...float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

// Observe は、時間 d を数える。
func (h *Histogram) Observe(d time.Duration) {
	s := d.Seconds()
	i := 0
	for i < len(h.bounds) && s > h.bounds[i] {
		i++
	}
	h.counts[i].Add(1)
	h.sum.Add(int64(d))
}

// Histogram は、h を書く。
func (w *Writer) Histogram(name, help string, h *Histogram) {
	w.header(name, "histogram", help)
	var total uint64
	for i, b := range h.bounds {
		total += h.counts[i].Load()
		w.sample(name, "", "", name+"_bucket", float64(total), []string{"le", formatValue(b)})
	}
	total += h.counts[len(h.bounds)].Load()
	w.sample(name, "", "", name+"_bucket", float64(total), []string{"le", "+Inf"})
	w.sample(name, "", "", name+"_sum", time.Duration(h.sum.Load()).Seconds(), nil)
	w.sample(name, "", "", name+"_count", float64(total), nil)
}

// Bytes は、書いた内容を返す。
func (w *Writer) Bytes() []byte { return w.buf.Bytes() }

// Handler は、GET /metrics に write が書いた指標を返す。ほかのパスは 404。
func Handler(write func(*Writer)) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(rw, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			rw.Header().Set("Allow", "GET, HEAD")
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var w Writer
		write(&w)
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = rw.Write(w.Bytes())
	})
}
