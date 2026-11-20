package main

import (
	"math"
	"sort"
	"time"
)

// sample は、1 つのページの結果。
type sample struct {
	page    string
	write   bool
	at      time.Duration // 測りの始めから、ページを終えた時点まで
	latency time.Duration // 決まった速さでは、予定の時刻から終えるまで（遅れを含む）。速さを決めないときは始めてから終えるまで
	service time.Duration // 始めてから終えるまで
	late    time.Duration // 予定の時刻から始めるまで（決まった速さのときだけ）
	stmts   int
	failed  bool
}

// Dist は、時間の分布（ミリ秒）。
type Dist struct {
	Count int     `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P95   float64 `json:"p95"`
	P99   float64 `json:"p99"`
	P999  float64 `json:"p999"`
	Max   float64 `json:"max"`
}

// percentile は、並べた値の p（0〜100）の値を、最も近い順位の値で返す（MySQL の性能の道具と同じ、補間しない形）。
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	r := int(math.Ceil(p / 100 * float64(len(sorted))))
	if r < 1 {
		r = 1
	}
	if r > len(sorted) {
		r = len(sorted)
	}
	return sorted[r-1]
}

func ms(d time.Duration) float64 { return math.Round(float64(d)/1e3) / 1e3 }

// distOf は、値の分布を返す。ds は並べ替える。
func distOf(ds []time.Duration) Dist {
	if len(ds) == 0 {
		return Dist{}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return Dist{
		Count: len(ds),
		Mean:  ms(sum / time.Duration(len(ds))),
		P50:   ms(percentile(ds, 50)),
		P90:   ms(percentile(ds, 90)),
		P95:   ms(percentile(ds, 95)),
		P99:   ms(percentile(ds, 99)),
		P999:  ms(percentile(ds, 99.9)),
		Max:   ms(ds[len(ds)-1]),
	}
}

// Interval は、時間の区切りごとの値（限界の先と長い時間の測りで、時間とともに変わるかを見る）。
type Interval struct {
	T          float64 `json:"t"` // 区切りの終わり（測りの始めからの秒）
	Pages      int     `json:"pages"`
	PagesPerS  float64 `json:"pages_per_s"`
	P50        float64 `json:"p50_ms"`
	P99        float64 `json:"p99_ms"`
	Max        float64 `json:"max_ms"`
	FailedPage int     `json:"failed_pages"`
}

// series は、終えた時刻で区切った値を返す。
func series(ss []sample, window, every time.Duration) []Interval {
	if every <= 0 || window <= 0 {
		return nil
	}
	n := int((window + every - 1) / every)
	buckets := make([][]time.Duration, n)
	failed := make([]int, n)
	for _, s := range ss {
		i := int(s.at / every)
		if i < 0 || i >= n {
			continue
		}
		buckets[i] = append(buckets[i], s.latency)
		if s.failed {
			failed[i]++
		}
	}
	out := make([]Interval, n)
	for i, b := range buckets {
		d := distOf(b)
		end := time.Duration(i+1) * every
		if end > window {
			end = window
		}
		span := (end - time.Duration(i)*every).Seconds()
		out[i] = Interval{T: end.Seconds(), Pages: d.Count, PagesPerS: math.Round(float64(d.Count)/span*100) / 100,
			P50: d.P50, P99: d.P99, Max: d.Max, FailedPage: failed[i]}
	}
	return out
}
