package main

import (
	"context"
	"time"
)

// A side statement (-side) runs on its own connection at fixed times during the measured window, for example
// CALL heimdb_backup() every 30 seconds (docs/bench.md, "During a backup"). The result lists each run, and splits
// the page times into the pages whose time overlapped a run and the others.

// SideRun is one run of the side statement.
type SideRun struct {
	StartS float64 `json:"start_s"` // from the start of the measured window
	EndS   float64 `json:"end_s"`
	Ms     float64 `json:"ms"`
	Error  string  `json:"error,omitempty"`
}

// runSide runs cfg.Side at mStart+SideFirst, then every SideEvery, until tEnd. A run that has not finished by
// the next time delays the next run (runs never overlap). A run that starts before tEnd is waited for, even past it.
func runSide(ctx context.Context, w *Workload, dial Dialer, cfg Config, mStart, tEnd time.Time) []SideRun {
	var runs []SideRun
	var c Conn
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	for k := 0; ; k++ {
		at := mStart.Add(cfg.SideFirst + time.Duration(k)*cfg.SideEvery)
		if !at.Before(tEnd) || !sleepUntil(ctx, at) {
			return runs
		}
		var err error
		if c == nil {
			c, err = dialSession(ctx, w, dial, cfg)
		}
		start := time.Now()
		if !start.Before(tEnd) {
			return runs
		}
		if err == nil {
			_, _, err = c.Run(ctx, cfg.Side, true, 0)
			if err != nil && mysqlNumber(err) == 0 {
				c.Close()
				c = nil
			}
		}
		end := time.Now()
		r := SideRun{StartS: start.Sub(mStart).Seconds(), EndS: end.Sub(mStart).Seconds(), Ms: ms(end.Sub(start))}
		if err != nil {
			r.Error = errKey(err)
		}
		runs = append(runs, r)
		if cfg.SideEvery <= 0 {
			return runs
		}
	}
}

func dialSession(ctx context.Context, w *Workload, dial Dialer, cfg Config) (Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()
	c, err := dial(dctx)
	if err != nil {
		return nil, err
	}
	for _, s := range w.Session {
		if _, _, err := c.Run(dctx, s, false, 0); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

// splitBySide returns the page time distributions ("all", "read", "write") of the pages whose time (from the
// intended start to the end) overlapped a side run, and of the other pages.
func splitBySide(all []sample, runs []SideRun) (during, outside map[string]Dist) {
	if len(runs) == 0 {
		return nil, nil
	}
	in, out := map[string][]time.Duration{}, map[string][]time.Duration{}
	for _, s := range all {
		end := s.at.Seconds()
		begin := (s.at - s.latency).Seconds()
		m := out
		for _, r := range runs {
			if begin < r.EndS && end > r.StartS {
				m = in
				break
			}
		}
		kind := "read"
		if s.write {
			kind = "write"
		}
		m["all"] = append(m["all"], s.latency)
		m[kind] = append(m[kind], s.latency)
	}
	during, outside = map[string]Dist{}, map[string]Dist{}
	for k, v := range in {
		during[k] = distOf(v)
	}
	for k, v := range out {
		outside[k] = distOf(v)
	}
	return during, outside
}
