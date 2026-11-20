// compatdiff は、MySQL 互換のテスト集（compat/README.md）が本物の MySQL で記録した値（ベースライン）と、調べる DB で
// 記録した値を比べ、違うものを一覧にする。
//
//	go run ./cmd/compatdiff compat/baseline/mysql-8.0.json heimdb.json
//
// 意図した違いは compat/known-differences.txt（-known で変えられる）に、キーと理由を書く。それ以外の違いと、片方に
// しかないキーがあれば終了コード 1 を返す。知っている違いのうち、違わなくなったものは知らせる（一覧から消す）。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
)

func load(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// loadKnown は、知っている違いの一覧を読む。1 行に「キー<タブ>理由」。# で始まる行と空の行は飛ばす。
func loadKnown(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	known := map[string]string{}
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, reason, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("%s:%d: want <key><TAB><reason>", path, n)
		}
		known[strings.TrimSpace(key)] = strings.TrimSpace(reason)
	}
	return known, s.Err()
}

// compact は、値を 1 行の JSON にする（比べるときも表示するときも同じ形）。
func compact(v json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, v); err != nil {
		return string(v)
	}
	return b.String()
}

func main() {
	knownPath := flag.String("known", "compat/known-differences.txt", "file of intended differences (<key><TAB><reason>)")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: compatdiff [-known file] <mysql.json> <target.json>")
		os.Exit(2)
	}
	base, err := load(flag.Arg(0))
	if err == nil {
		var target map[string]json.RawMessage
		if target, err = load(flag.Arg(1)); err == nil {
			var known map[string]string
			if known, err = loadKnown(*knownPath); err == nil {
				os.Exit(report(base, target, known))
			}
		}
	}
	fmt.Fprintln(os.Stderr, err)
	os.Exit(2)
}

func report(base, target map[string]json.RawMessage, known map[string]string) int {
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range target {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	unexpected, expected, fixed := 0, 0, 0
	for _, k := range sorted {
		b, inBase := base[k]
		t, inTarget := target[k]
		reason, isKnown := known[k]
		switch {
		case !inBase || !inTarget:
			where := "MySQL"
			if inTarget {
				where = "the target"
			}
			fmt.Printf("ONLY IN %s: %s\n", where, k)
			unexpected++
		case compact(b) == compact(t):
			if isKnown {
				fmt.Printf("NOW SAME (remove from the known list): %s\n", k)
				fixed++
			}
		case isKnown:
			fmt.Printf("known: %s (%s)\n", k, reason)
			expected++
		default:
			fmt.Printf("DIFFERENT: %s\n  mysql:  %s\n  target: %s\n", k, compact(b), compact(t))
			unexpected++
		}
	}
	fmt.Printf("%d observations, %d unexpected, %d known, %d known but now the same\n", len(sorted), unexpected, expected, fixed)
	if unexpected > 0 {
		return 1
	}
	return 0
}
