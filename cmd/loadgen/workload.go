package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
)

// Workload は、ページの文の並び（-pages の JSON）。
//
//	{
//	  "session": ["SET NAMES ...", "SET SESSION sql_mode = ..."],
//	  "setup":   [{"check": "SELECT ...", "steps": [...]}],
//	  "pages":   [{"name": "top", "kind": "read", "steps": [{"sql": "SELECT ..."}]},
//	              {"name": "post_create", "kind": "write", "steps": [{"sql": "INSERT ... {{u}}", "save_id": "post"}]}]
//	}
//
// session は接続ごとに最初に 1 回流す。setup は測りの前に 1 本の接続で 1 回だけ、check の文が行を返さないときに steps を流す
// （書き込みのページが前提にするタグなど）。文の中の {{名前}} は、流す前に値に置き換える:
// u はページごとに違う数（ページを始めるたびに増える。測りのたびに別の基点から数える）、now は今の UTC の日時
// （'2006-01-02 15:04:05' の形）、ほかは同じ接続の前の文で取っておいた値。save_id はその文の LAST_INSERT_ID を、
// save は最初の行の列を前から順に、その名前で取っておく。取っておく文が失敗したら、そのページの残りを流さない。
type Workload struct {
	Session []string    `json:"session"`
	Setup   []SetupStep `json:"setup"`
	Pages   []*Page     `json:"pages"`

	reads, writes []*Page
}

// SetupStep は、check が行を返さないときに流す文。
type SetupStep struct {
	Check string  `json:"check"`
	Steps []*Step `json:"steps"`
}

// Page は、1 本の接続で順に流す文の並び。kind は read か write。
type Page struct {
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	Steps []*Step `json:"steps"`
}

// Step は、1 つの文。
type Step struct {
	SQL    string   `json:"sql"`
	SaveID string   `json:"save_id,omitempty"`
	Save   []string `json:"save,omitempty"`

	parts []string // 文字と変数の名前が交互（偶数番目が文字）
	query bool     // 行を返す文（SELECT、SHOW）
}

// LoadWorkload は、JSON のファイルを読む。並びの JSON（文字列の配列）は、read のページ 1 つとして読む。
func LoadWorkload(path string) (*Workload, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseWorkload(b)
}

// ParseWorkload は、Workload の JSON を読み、文を前もって分けておく。
func ParseWorkload(b []byte) (*Workload, error) {
	w := &Workload{}
	var list []string
	if json.Unmarshal(b, &list) == nil {
		p := &Page{Name: "page", Kind: "read"}
		for _, s := range list {
			p.Steps = append(p.Steps, &Step{SQL: s})
		}
		w.Pages = []*Page{p}
	} else if err := json.Unmarshal(b, w); err != nil {
		return nil, err
	}
	for _, p := range w.Pages {
		switch p.Kind {
		case "read":
			w.reads = append(w.reads, p)
		case "write":
			w.writes = append(w.writes, p)
		default:
			return nil, fmt.Errorf("page %q: kind must be read or write, got %q", p.Name, p.Kind)
		}
		if len(p.Steps) == 0 {
			return nil, fmt.Errorf("page %q has no statements", p.Name)
		}
		for _, s := range p.Steps {
			if err := s.compile(); err != nil {
				return nil, fmt.Errorf("page %q: %w", p.Name, err)
			}
		}
	}
	for _, su := range w.Setup {
		for _, s := range su.Steps {
			if err := s.compile(); err != nil {
				return nil, fmt.Errorf("setup: %w", err)
			}
		}
	}
	return w, nil
}

func (s *Step) compile() error {
	rest := s.SQL
	s.parts = nil
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			s.parts = append(s.parts, rest)
			break
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return fmt.Errorf("unclosed {{ in %.60q", s.SQL)
		}
		name := strings.TrimSpace(rest[i+2 : i+j])
		if name == "" {
			return fmt.Errorf("empty {{}} in %.60q", s.SQL)
		}
		s.parts = append(s.parts, rest[:i], name)
		rest = rest[i+j+2:]
	}
	w := strings.ToUpper(strings.TrimLeft(s.SQL, " \t\r\n("))
	s.query = strings.HasPrefix(w, "SELECT") || strings.HasPrefix(w, "SHOW") || strings.HasPrefix(w, "DESCRIBE") || strings.HasPrefix(w, "EXPLAIN")
	return nil
}

// render は、変数を置き換えた文を返す。知らない変数は 0 にする（前のページの作成が失敗したときなど）。
func (s *Step) render(vars map[string]string) string {
	if len(s.parts) == 1 {
		return s.parts[0]
	}
	var b strings.Builder
	for i, p := range s.parts {
		if i%2 == 0 {
			b.WriteString(p)
			continue
		}
		v, ok := vars[p]
		if !ok {
			v = "0"
		}
		b.WriteString(v)
	}
	return b.String()
}

// Mixer は、1 本の接続が次に流すページを選ぶ。書き込みの割合 writeFrac で読みか書き込みかを決め、
// 読みのページは順に回し（接続ごとに始めをずらす）、書き込みのページはファイルの順に回す（作成、コメント、更新、削除が
// 同じ接続の中でこの順になり、削除はその接続が作った記事を消す）。
type Mixer struct {
	w         *Workload
	writeFrac float64
	rng       *rand.Rand
	ri, wi    int
}

// NewMixer は、接続 conn の Mixer を作る。
func NewMixer(w *Workload, writeFrac float64, seed uint64, conn int) *Mixer {
	m := &Mixer{w: w, writeFrac: writeFrac, rng: rand.New(rand.NewPCG(seed, uint64(conn)))}
	if len(w.reads) > 0 {
		m.ri = conn % len(w.reads)
	}
	return m
}

// Next は、次のページを返す。
func (m *Mixer) Next() *Page {
	write := len(m.w.writes) > 0 && (len(m.w.reads) == 0 || m.rng.Float64() < m.writeFrac)
	if write {
		p := m.w.writes[m.wi%len(m.w.writes)]
		m.wi++
		return p
	}
	p := m.w.reads[m.ri%len(m.w.reads)]
	m.ri++
	return p
}

// Merge は、b のページを w に足す（読みのファイルと書き込みのファイルを分けて渡すとき）。session と setup は空でない方を使う。
func (w *Workload) Merge(b *Workload) {
	if len(w.Session) == 0 {
		w.Session = b.Session
	}
	w.Setup = append(w.Setup, b.Setup...)
	w.Pages = append(w.Pages, b.Pages...)
	w.reads = append(w.reads, b.reads...)
	w.writes = append(w.writes, b.writes...)
}
