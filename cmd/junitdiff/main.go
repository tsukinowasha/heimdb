// junitdiff は、本物の MySQL で取った JUnit の結果（ベースライン）と HeimDB での結果を比べ、
// 本物で成功し HeimDB で失敗したテストだけを一覧にする。
//
//	go run ./cmd/junitdiff real-option.xml fake-option.xml
//
// 差がなければ終了コード 0、あれば 1 を返す。
package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"sort"
	"strings"
)

type suite struct {
	Suites []suite    `xml:"testsuite"`
	Cases  []testcase `xml:"testcase"`
}

type testcase struct {
	Name    string    `xml:"name,attr"`
	Class   string    `xml:"class,attr"`
	File    string    `xml:"file,attr"`
	Line    string    `xml:"line,attr"`
	Failure *message  `xml:"failure"`
	Error   *message  `xml:"error"`
	Skipped *struct{} `xml:"skipped"`
}

type message struct {
	Text string `xml:",chardata"`
}

// outcome はテストの結果。
type outcome struct {
	status string // pass、fail、error、skip
	detail string
	where  string
}

func load(path string) (map[string]outcome, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var root struct {
		Suites []suite `xml:"testsuite"`
	}
	if err := xml.NewDecoder(f).Decode(&root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]outcome{}
	var walk func(s suite)
	walk = func(s suite) {
		for _, c := range s.Cases {
			o := outcome{status: "pass", where: c.File + ":" + c.Line}
			switch {
			case c.Error != nil:
				o.status, o.detail = "error", c.Error.Text
			case c.Failure != nil:
				o.status, o.detail = "fail", c.Failure.Text
			case c.Skipped != nil:
				o.status = "skip"
			}
			out[c.Class+"::"+c.Name] = o
		}
		for _, child := range s.Suites {
			walk(child)
		}
	}
	for _, s := range root.Suites {
		walk(s)
	}
	return out, nil
}

func count(m map[string]outcome) map[string]int {
	c := map[string]int{}
	for _, o := range m {
		c[o.status]++
	}
	return c
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return "      " + strings.Join(lines, "\n      ")
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: junitdiff BASELINE.xml HEIMDB.xml")
		os.Exit(2)
	}
	base, err := load(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	got, err := load(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	bc, gc := count(base), count(got)
	fmt.Printf("baseline: %d tests (pass %d, fail %d, error %d, skip %d)\n", len(base), bc["pass"], bc["fail"], bc["error"], bc["skip"])
	fmt.Printf("HeimDB:   %d tests (pass %d, fail %d, error %d, skip %d)\n", len(got), gc["pass"], gc["fail"], gc["error"], gc["skip"])

	var regressions, missing []string
	for name, b := range base {
		g, ok := got[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if b.status == "pass" && (g.status == "fail" || g.status == "error") {
			regressions = append(regressions, name)
		}
	}
	sort.Strings(regressions)
	sort.Strings(missing)

	fmt.Printf("\npassed on MySQL but failed on HeimDB: %d\n", len(regressions))
	for _, name := range regressions {
		g := got[name]
		fmt.Printf("  [%s] %s (%s)\n%s\n", g.status, name, g.where, firstLines(g.detail, 6))
	}
	if len(missing) > 0 {
		fmt.Printf("\nin baseline but not run on HeimDB: %d\n", len(missing))
		for _, name := range missing {
			fmt.Printf("  %s\n", name)
		}
	}
	if len(regressions) > 0 || len(missing) > 0 {
		os.Exit(1)
	}
}
