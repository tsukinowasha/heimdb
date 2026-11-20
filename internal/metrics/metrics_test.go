package metrics

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// 書いた指標が Prometheus の解析器を通り、値、ラベル、ヒストグラムの累積が正しい（docs/plan.md の段階 73）。
func TestWriter(t *testing.T) {
	h := NewHistogram(0.01, 1)
	h.Observe(5 * time.Millisecond)
	h.Observe(10 * time.Millisecond) // 境目ちょうどは、その境目に入る
	h.Observe(500 * time.Millisecond)
	h.Observe(2 * time.Second)
	var w Writer
	w.Gauge("g", "a gauge", 1.5)
	w.Counter("c_total", "a counter\nwith \\ in help", 3, "type", "select")
	w.Counter("c_total", "a counter\nwith \\ in help", 4, "type", `say "hi"\`+"\n")
	w.Histogram("d_seconds", "a histogram", h)
	fams, err := textParser().TextToMetricFamilies(bytes.NewReader(w.Bytes()))
	if err != nil {
		t.Fatalf("%v\n%s", err, w.Bytes())
	}
	if v := fams["g"].Metric[0].GetGauge().GetValue(); v != 1.5 {
		t.Errorf("g = %v", v)
	}
	c := fams["c_total"].Metric
	if len(c) != 2 || c[0].GetCounter().GetValue() != 3 || c[1].Label[0].GetValue() != `say "hi"\`+"\n" || c[1].GetCounter().GetValue() != 4 {
		t.Errorf("c_total = %v", c)
	}
	d := fams["d_seconds"].Metric[0].GetHistogram()
	if d.GetSampleCount() != 4 || d.GetSampleSum() < 2.51 || d.GetSampleSum() > 2.52 {
		t.Errorf("count %d, sum %v", d.GetSampleCount(), d.GetSampleSum())
	}
	var counts []uint64
	for _, b := range d.Bucket {
		counts = append(counts, b.GetCumulativeCount())
	}
	if len(counts) != 3 || counts[0] != 2 || counts[1] != 3 || counts[2] != 4 {
		t.Errorf("buckets %v\n%s", counts, w.Bytes())
	}
}

// /metrics だけを GET と HEAD で返し、ほかのパスは 404、ほかの方法は 405。
func TestHandler(t *testing.T) {
	srv := httptest.NewServer(Handler(func(w *Writer) { w.Gauge("up", "always 1", 1) }))
	defer srv.Close()
	get := func(method, path string) (int, string, string) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), string(b)
	}
	if code, typ, body := get("GET", "/metrics"); code != 200 || !strings.HasPrefix(typ, "text/plain; version=0.0.4") || !strings.Contains(body, "up 1\n") {
		t.Errorf("GET /metrics: %d %q %q", code, typ, body)
	}
	if code, _, _ := get("HEAD", "/metrics"); code != 200 {
		t.Errorf("HEAD: %d", code)
	}
	for _, p := range []string{"/", "/metrics/", "/debug/pprof/"} {
		if code, _, _ := get("GET", p); code != 404 {
			t.Errorf("GET %s: %d", p, code)
		}
	}
	if code, _, _ := get("POST", "/metrics"); code != 405 {
		t.Errorf("POST: %d", code)
	}
}

// textParser は、Prometheus の文字の形を読む。名前は旧来の規則（英数字と _ と :）で確かめる。
func textParser() *expfmt.TextParser {
	p := expfmt.NewTextParser(model.LegacyValidation)
	return &p
}
