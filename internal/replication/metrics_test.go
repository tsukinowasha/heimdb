package replication

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/tsukinowasha/heimdb/internal/metrics"
)

// gauges は、write が書いた指標を解析器に通し、名前（ラベルがあれば name{value}）から値へのマップにする。
func gauges(t *testing.T, write func(*metrics.Writer)) map[string]float64 {
	t.Helper()
	var w metrics.Writer
	write(&w)
	fams, err := textParser().TextToMetricFamilies(bytes.NewReader(w.Bytes()))
	if err != nil {
		t.Fatalf("%v\n%s", err, w.Bytes())
	}
	out := map[string]float64{}
	for name, f := range fams {
		for _, m := range f.Metric {
			key := name
			if len(m.Label) > 0 {
				key += "{" + m.Label[0].GetValue() + "}"
			}
			out[key] = m.GetGauge().GetValue() + m.GetCounter().GetValue()
		}
	}
	return out
}

// 本番はレプリカごとの遅れと最後の連絡、同期の状態を、レプリカは本番からの遅れ、最後の連絡、エラーを出す
// （docs/plan.md の段階 73）。
func TestReplicationMetrics(t *testing.T) {
	p, r1 := openPair(t)
	r2 := openReplica(t, p)
	prim := NewPrimary(p, "tok", 300*time.Millisecond)
	hs := httptest.NewServer(prim)
	defer hs.Close()
	defer prim.Close()
	srv := hs.URL
	rep1, err := NewReplica(r1, srv, "tok", "r1", nil)
	if err != nil {
		t.Fatal(err)
	}
	rep2, err := NewReplica(r2, srv, "tok", "r2", nil)
	if err != nil {
		t.Fatal(err)
	}
	stop1 := runReplica(rep1)
	defer stop1()
	stop2 := runReplica(rep2)
	waitMode(t, prim, "sync")
	if err := p.SetUsers([]byte("both")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	stop2()
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if err := p.SetUsers([]byte(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	m := gauges(t, prim.WriteMetrics)
	if m["heimdb_replication_sync"] != 1 || m["heimdb_replica_behind_seconds{r1}"] != 0 || m["heimdb_replica_behind_seconds{r2}"] <= 0 ||
		m["heimdb_replica_last_seen_seconds{r2}"] < 0.1 || m["heimdb_replica_last_seen_seconds{r1}"] > m["heimdb_replica_last_seen_seconds{r2}"] ||
		m["heimdb_replication_async_switches_total"] != 0 {
		t.Errorf("primary: %v", m)
	}
	if m := gauges(t, rep1.WriteMetrics); m["heimdb_replication_behind_seconds"] != 0 || m["heimdb_replication_failing"] != 0 || m["heimdb_replication_last_contact_seconds"] > 1 {
		t.Errorf("r1: %v", m)
	}
	// 止まったレプリカは失敗として出す（合言葉が違う）。
	bad, err := NewReplica(openReplica(t, p), srv, "wrong", "bad", nil)
	if err != nil {
		t.Fatal(err)
	}
	stopBad := runReplica(bad)
	defer stopBad()
	deadline := time.Now().Add(5 * time.Second)
	for gauges(t, bad.WriteMetrics)["heimdb_replication_failing"] != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("bad replica: %v", bad.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// textParser は、Prometheus の文字の形を読む。名前は旧来の規則（英数字と _ と :）で確かめる。
func textParser() *expfmt.TextParser {
	p := expfmt.NewTextParser(model.LegacyValidation)
	return &p
}
