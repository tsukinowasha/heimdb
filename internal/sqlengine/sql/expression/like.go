// Copyright 2020-2021 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Modified by Tsukinowa Inc.: `%` だけを含むパターンを区間ごとに探して照合する（docs/engine.md）
// Modified by Tsukinowa Inc.: U+FFFD（置き換えの文字）を壊れた UTF-8 とみなさない（上流の #3556。docs/engine.md）
// Modified by Tsukinowa Inc.: ENUM と SET の列の値を、文字列の文脈で値の文字列として扱う（上流の #3068。docs/engine.md）
// Modified by Tsukinowa Inc.: LIKE の日付と日時を型の形の文字列にする（段階 116。docs/engine.md）

package expression

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// Like performs pattern matching against two strings.
type Like struct {
	BinaryExpressionStub
	Escape sql.Expression
	pool   *sync.Pool
	once   sync.Once
	cached bool
}

var _ sql.Expression = (*Like)(nil)
var _ sql.CollationCoercible = (*Like)(nil)

type likeMatcherErrTuple struct {
	matcher LikeMatcher
	err     error
}

// NewLike creates a new LIKE expression.
func NewLike(left, right, escape sql.Expression) sql.Expression {
	var cached = true
	sql.Inspect(right, func(e sql.Expression) bool {
		if _, ok := e.(*GetField); ok {
			cached = false
		}
		return true
	})

	return &Like{
		BinaryExpressionStub: BinaryExpressionStub{left, right},
		Escape:               escape,
		pool:                 nil,
		once:                 sync.Once{},
		cached:               cached,
	}
}

// Type implements the sql.Expression interface.
func (l *Like) Type() sql.Type { return types.Boolean }

// CollationCoercibility implements the interface sql.CollationCoercible.
func (l *Like) CollationCoercibility(ctx *sql.Context) (collation sql.CollationID, coercibility byte) {
	leftCollation, leftCoercibility := sql.GetCoercibility(ctx, l.LeftChild)
	rightCollation, rightCoercibility := sql.GetCoercibility(ctx, l.RightChild)
	return sql.ResolveCoercibility(leftCollation, leftCoercibility, rightCollation, rightCoercibility)
}

// Eval implements the sql.Expression interface.
func (l *Like) Eval(ctx *sql.Context, row sql.Row) (interface{}, error) {
	span, ctx := ctx.Span("expression.Like")
	defer span.End()

	left, err := l.LeftChild.Eval(ctx, row)
	if err != nil || left == nil {
		return nil, err
	}
	left, err = sql.UnwrapAny(ctx, left)
	if err != nil {
		return nil, err
	}
	// ENUM と SET は番号ではなく値の文字列（上流の #3068）
	if str, ok := types.ValueString(l.LeftChild.Type(), left); ok {
		left = str
	}
	if _, ok := left.(string); !ok {
		left, _, err = types.LongText.Convert(ctx, left)
		if err != nil {
			return nil, err
		}
	}

	var lm LikeMatcher
	right, escape, err := l.evalRight(ctx, row)
	if err != nil {
		return nil, err
	}
	if right == nil {
		return nil, nil
	}
	if !l.cached {
		// for non-cached regex every time create a new matcher
		collation, _ := l.CollationCoercibility(ctx)
		lm, err = ConstructLikeMatcher(collation, *right, escape)
	} else {
		l.once.Do(func() {
			l.pool = &sync.Pool{
				New: func() interface{} {
					collation, _ := l.CollationCoercibility(ctx)
					m, e := ConstructLikeMatcher(collation, *right, escape)
					return likeMatcherErrTuple{m, e}
				},
			}
		})
		tpl := l.pool.Get().(likeMatcherErrTuple)
		lm, err = tpl.matcher, tpl.err
	}
	if err != nil {
		return nil, err
	}
	if lm.collation == sql.Collation_Unspecified {
		return false, nil
	}

	ok := lm.Match(left.(string))
	if l.cached {
		l.pool.Put(likeMatcherErrTuple{lm, nil})
	}
	return ok, nil
}

func (l *Like) evalRight(ctx *sql.Context, row sql.Row) (right *string, escape rune, err error) {
	rightVal, err := l.RightChild.Eval(ctx, row)
	if err != nil || rightVal == nil {
		return nil, 0, err
	}
	rightVal, err = sql.UnwrapAny(ctx, rightVal)
	if err != nil {
		return nil, 0, err
	}
	if str, ok := types.ValueString(l.RightChild.Type(), rightVal); ok {
		rightVal = str
	}
	if _, ok := rightVal.(string); !ok {
		rightVal, _, err = types.LongText.Convert(ctx, rightVal)
		if err != nil {
			return nil, 0, err
		}
	}

	var escapeVal interface{}
	if l.Escape != nil {
		escapeVal, err = l.Escape.Eval(ctx, row)
		if err != nil {
			return nil, 0, err
		}
		if escapeVal == nil {
			escapeVal = `\`
		}
		if _, ok := escapeVal.(string); !ok {
			escapeVal, _, err = types.LongText.Convert(ctx, escapeVal)
			if err != nil {
				return nil, 0, err
			}
		}
		if utf8.RuneCountInString(escapeVal.(string)) > 1 {
			return nil, 0, sql.ErrInvalidArgument.New("ESCAPE")
		}
	} else {
		escapeVal = `\`
	}

	rightStr := rightVal.(string)
	return &rightStr, []rune(escapeVal.(string))[0], nil
}

func (l *Like) String() string {
	return fmt.Sprintf("%s LIKE %s", l.LeftChild, l.RightChild)
}

// WithChildren implements the Expression interface.
func (l *Like) WithChildren(children ...sql.Expression) (sql.Expression, error) {
	if len(children) != 2 {
		return nil, sql.ErrInvalidChildrenNumber.New(l, len(children), 2)
	}
	return NewLike(children[0], children[1], l.Escape), nil
}

func patternToGoRegex(pattern string) string {
	var buf bytes.Buffer
	buf.WriteString("(?s)")
	buf.WriteRune('^')
	var escaped bool
	for _, r := range strings.Replace(regexp.QuoteMeta(pattern), `\\`, `\`, -1) {
		switch r {
		case '_':
			if escaped {
				buf.WriteRune(r)
			} else {
				buf.WriteRune('.')
			}
		case '%':
			if escaped {
				buf.WriteRune(r)
			} else {
				buf.WriteString(".*")
			}
		case '\\':
			if escaped {
				buf.WriteString(`\\`)
			} else {
				escaped = true
				continue
			}
		default:
			if escaped {
				buf.WriteString(`\`)
			}
			buf.WriteRune(r)
		}

		if escaped {
			escaped = false
		}
	}

	buf.WriteRune('$')
	return buf.String()
}

func patternToGoRegexWithEscape(pattern, escape string) string {
	var buf bytes.Buffer
	buf.WriteString("(?s)")
	buf.WriteRune('^')
	var escaped bool

	for _, r := range strings.Replace(strings.Replace(regexp.QuoteMeta(pattern), `\\`, `\`, -1), regexp.QuoteMeta(escape), escape, -1) {
		switch r {
		case rune(escape[0]):
			if escaped {
				buf.WriteString(regexp.QuoteMeta(escape))
			} else {
				escaped = true
				continue
			}
		case '_':
			if escaped {
				buf.WriteRune(r)
			} else {
				buf.WriteRune('.')
			}
		case '%':
			if escaped {
				buf.WriteRune(r)
			} else {
				buf.WriteString(".*")
			}
		case '\\':
			if escaped {
				buf.WriteString(`\\`)
			} else {
				escaped = true
				continue
			}
		default:
			if escaped {
				buf.WriteString(`\`)
			}
			buf.WriteRune(r)
		}

		if escaped {
			escaped = false
		}
	}

	buf.WriteRune('$')
	return buf.String()
}

// LikeMatcher is a collation-supported matcher for LIKE expressions.
type LikeMatcher struct {
	nodes     []likeMatcherNode
	collation sql.CollationID
	escape    rune
	// fast は、パターンが `%` と普通の文字だけのときの照合（HeimDB。nil なら nodes で照合する）。
	fast *likeSegments
}

// ConstructLikeMatcher returns a new LikeMatcher.
func ConstructLikeMatcher(collation sql.CollationID, pattern string, escape rune) (LikeMatcher, error) {
	charsetEncoder := collation.CharacterSet().Encoder()
	sorter := collation.Sorter()
	matcher := LikeMatcher{nil, collation, escape, nil}
	for i := 0; i < len(pattern); {
		nextRune, advance := charsetEncoder.NextRune(pattern[i:])
		// NextRune が utf8.RuneError を 1 バイト以下で返すのは壊れたバイト列だけ。置き換えの文字 U+FFFD は正しい
		// 文字で 3 バイトで読めるので、ここで拒否しない（上流の #3556）
		if nextRune == utf8.RuneError && advance <= 1 {
			return LikeMatcher{}, sql.ErrCharSetInvalidString.New(collation.CharacterSet().Name(), pattern)
		}
		i += advance

		switch nextRune {
		case '_': // Matches any single character
			matcher.nodes = append(matcher.nodes, likeMatcherRune{'_', -1})
		case '%': // Matches any sequence of characters, including the empty sequence
			matcher.nodes = append(matcher.nodes, likeMatcherAny{})
		case escape: // States that the next character should be taken literally
			nextRune, advance = charsetEncoder.NextRune(pattern[i:])
			if nextRune == utf8.RuneError && advance <= 1 {
				return LikeMatcher{}, sql.ErrCharSetInvalidString.New(collation.CharacterSet().Name(), pattern)
			}
			i += advance
			matcher.nodes = append(matcher.nodes, likeMatcherRune{nextRune, sorter(nextRune)})
		default: // A regular character that we'll match against
			matcher.nodes = append(matcher.nodes, likeMatcherRune{nextRune, sorter(nextRune)})
		}
	}
	matcher.fast = newLikeSegments(collation, matcher.nodes)
	return matcher, nil
}

// Match returns whether the given string conforms to the nodes contained in this matcher.
func (l LikeMatcher) Match(s string) bool {
	if l.fast != nil {
		return l.fast.match(s)
	}
	return l.matchNodes(s)
}

// matchNodes は、ノードを 1 つずつ照合し、合わなければ後戻りする元の照合（HeimDB: likeSegments と比べるテストのため
// 名前を分けた）。
func (l LikeMatcher) matchNodes(s string) bool {
	if len(l.nodes) == 0 {
		if len(s) == 0 {
			return true
		}
		return false
	}

	charsetEncoder := l.collation.CharacterSet().Encoder()
	stringIndex := 0
	nodeIndex := 0
	nodeNextIndex := make([]int, 0, len(l.nodes))
	for {
		// If both indexes equal their lengths, we've fully matched the string with all nodes
		if stringIndex == len(s) && nodeIndex == len(l.nodes) {
			return true
		}
		// If all nodes have found a match but we still have runes left in the string, we backtrack to allow earlier
		// nodes to match more runes. If we're unable to backtrack, then the string does not match.
		if stringIndex < len(s) && nodeIndex == len(l.nodes) {
			var matched bool
			matched, nodeIndex = l.backtrack(s, nodeIndex-1, nodeNextIndex)
			if !matched {
				return false
			}
			nodeNextIndex = nodeNextIndex[:nodeIndex]
			stringIndex = nodeNextIndex[nodeIndex-1]
			continue
		}
		// If all runes have found a match but we still have nodes left in the matcher, we check if the remaining nodes
		// are all "any sequence" nodes. If they're not, then the string is too short and does not match.
		if stringIndex == len(s) && nodeIndex < len(l.nodes) {
			for ; nodeIndex < len(l.nodes); nodeIndex++ {
				if _, ok := l.nodes[nodeIndex].(likeMatcherAny); !ok {
					return false
				}
			}
			return true
		}

		nextRune, advance := charsetEncoder.NextRune(s[stringIndex:])
		if nextRune == utf8.RuneError && advance <= 1 {
			return false
		}
		matched, consumed := l.nodes[nodeIndex].Match(l.collation, nextRune)
		if consumed {
			stringIndex += advance
		}
		if matched {
			nodeNextIndex = append(nodeNextIndex, stringIndex)
			nodeIndex++
		} else {
			// If we didn't match on this rune, we backtrack to allow earlier nodes to match more runes
			matched, nodeIndex = l.backtrack(s, nodeIndex, nodeNextIndex)
			if !matched {
				return false
			}
			nodeNextIndex = nodeNextIndex[:nodeIndex]
			stringIndex = nodeNextIndex[nodeIndex-1]
			continue
		}
	}
	// Must return something here to compile, but the above loop will handle all return cases
	return false
}

// String returns the string form of this LIKE expression. If an Escape character was provided, it is used instead of
// the default.
func (l LikeMatcher) String() string {
	sb := strings.Builder{}
	for _, node := range l.nodes {
		switch node := node.(type) {
		case likeMatcherRune:
			if node.original == '%' {
				sb.WriteRune(l.escape)
				sb.WriteRune('%')
			} else if node.original == '_' {
				if node.sortOrder != -1 {
					sb.WriteRune(l.escape)
				}
				sb.WriteRune('_')
			} else {
				sb.WriteRune(node.original)
			}
		case likeMatcherAny:
			sb.WriteRune('%')
		}
	}
	return sb.String()
}

// backtrack unwinds the stack until we can find a node that can match the next rune compared to the rune that it last
// matched against. The returned node index is the index to use for the next match.
func (l LikeMatcher) backtrack(s string, nodeIndex int, nodeNextIndex []int) (matched bool, newNodeIndex int) {
	charsetEncoder := l.collation.CharacterSet().Encoder()
	// If the slice doesn't contain an entry for the node, then that node was never matched (and therefore we can't
	// backtrack over it).
	if nodeIndex >= len(nodeNextIndex) {
		nodeIndex = len(nodeNextIndex) - 1
	}
	for ; nodeIndex >= 0; nodeIndex-- {
		stringIndex := nodeNextIndex[nodeIndex]
		nextRune, advance := charsetEncoder.NextRune(s[stringIndex:])
		if nextRune == utf8.RuneError && advance <= 1 {
			return false, 0
		}
		if l.nodes[nodeIndex].MatchNext(l.collation, nextRune) {
			nodeNextIndex[nodeIndex] = stringIndex + advance
			return true, nodeIndex + 1
		}
	}
	// We exhausted all nodes, no nodes may match further
	return false, 0
}

// likeMatcherNode handles the match characteristics for a particular character from the pattern.
type likeMatcherNode interface {
	// Match returns whether the given rune is matched on the initial match, and also whether this rune is consumed. If
	// not consumed, the same rune will be given to the next node. It is assumed that consuming a rune always matches
	// the rune.
	Match(collation sql.CollationID, r rune) (matched bool, consumed bool)
	// MatchNext returns whether the given rune is matched on a subsequent match. Only the first match may optionally
	// consume a rune, all subsequent matches will consume the rune.
	MatchNext(collation sql.CollationID, r rune) bool
}

// likeMatcherRune matches exactly one rune. If the sort order is negative, then this matches any rune (but still only
// a single rune).
type likeMatcherRune struct {
	original  rune
	sortOrder int32
}

var _ likeMatcherNode = likeMatcherRune{}

// Match implements the interface likeMatcherNode.
func (l likeMatcherRune) Match(collation sql.CollationID, r rune) (matched bool, consumed bool) {
	if l.sortOrder < 0 || collation.Sorter()(r) == l.sortOrder {
		return true, true
	}
	return false, false
}

// MatchNext implements the interface likeMatcherNode. As this only matches a single rune, all subsequent matches will
// fail.
func (l likeMatcherRune) MatchNext(collation sql.CollationID, r rune) bool {
	return false
}

// likeMatcherAny matches any sequence of characters, including the empty sequence.
type likeMatcherAny struct{}

var _ likeMatcherNode = likeMatcherAny{}

// Match implements the interface likeMatcherNode. This node is a reluctant matcher, meaning it attempts to match as few
// runes as possible. As this will always match the empty sequence first, we'll return true on the match, but will not
// consume the given rune.
func (l likeMatcherAny) Match(collation sql.CollationID, r rune) (matched bool, consumed bool) {
	return true, false
}

// MatchNext implements the interface likeMatcherNode.
func (l likeMatcherAny) MatchNext(collation sql.CollationID, r rune) bool {
	return true
}

// likeSegments は、`%` と普通の文字だけのパターン（WordPress の検索の `'%語%'` など）を、`%` で区切った区間ごとの
// 重みの列として持つ。HeimDB: 元の照合は 1 文字ごとにノードをインターフェースで呼び、一致しない位置ごとに後戻りする
// ので、検索の全件の LIKE の時間の大半を占めていた（docs/plan.md の段階 54）。`%` だけのワイルドカードでは、真ん中の
// 区間を左から順に最初に一致する位置で探し、最後の区間を末尾で確かめれば、後戻りと同じ結果になる。
type likeSegments struct {
	// segs は、`%` で区切った区間の重みの列。`%` が n 個なら n+1 個（空の区間を含む）。
	segs   [][]int32
	ascii  *[128]int32
	sorter sql.CollationSorter
}

// newLikeSegments は、ノードが `%` と重みを持つ文字だけで、文字セットが utf8mb4（1 文字を utf8 で読む）なら
// likeSegments を返す。`_` を含むパターンとほかの文字セットは nil（元の照合を使う）。
func newLikeSegments(collation sql.CollationID, nodes []likeMatcherNode) *likeSegments {
	if collation.CharacterSet() != sql.CharacterSet_utf8mb4 || collation.Sorter() == nil || collation.ASCIIWeights() == nil {
		return nil
	}
	l := &likeSegments{segs: [][]int32{nil}, ascii: collation.ASCIIWeights(), sorter: collation.Sorter()}
	for _, node := range nodes {
		switch node := node.(type) {
		case likeMatcherAny:
			l.segs = append(l.segs, nil)
		case likeMatcherRune:
			if node.sortOrder < 0 {
				return nil
			}
			last := len(l.segs) - 1
			l.segs[last] = append(l.segs[last], node.sortOrder)
		default:
			return nil
		}
	}
	return l
}

// weight は、s の i バイト目からの 1 文字の重みとバイト数を返す。
func (l *likeSegments) weight(s string, i int) (int32, int) {
	if c := s[i]; c < utf8.RuneSelf {
		return l.ascii[c], 1
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	return l.sorter(r), n
}

// prefix は、s の i バイト目から seg が一致すれば、一致した後の位置を返す。一致しなければ -1。
func (l *likeSegments) prefix(s string, i int, seg []int32) int {
	for _, w := range seg {
		if i >= len(s) {
			return -1
		}
		got, n := l.weight(s, i)
		if got != w {
			return -1
		}
		i += n
	}
	return i
}

// index は、s の i バイト目から後で seg が最初に一致する位置の後ろを返す。一致しなければ -1。
func (l *likeSegments) index(s string, i int, seg []int32) int {
	first, rest := seg[0], seg[1:]
	ascii := l.ascii
	for i < len(s) {
		var w int32
		n := 1
		if c := s[i]; c < utf8.RuneSelf {
			w = ascii[c]
		} else {
			var r rune
			r, n = utf8.DecodeRuneInString(s[i:])
			w = l.sorter(r)
		}
		i += n
		if w == first {
			if end := l.prefix(s, i, rest); end >= 0 {
				return end
			}
		}
	}
	return -1
}

// match は、元の照合（matchNodes）と同じ結果を返す。元の照合は、壊れた UTF-8 のバイト列を読むと偽にし（U+FFFD は
// 正しい文字として照合する。上流の #3556）、真になるのは文字列を最後まで読んだときだけなので、一致したときだけ確かめる。
func (l *likeSegments) match(s string) bool {
	return l.matchSegments(s) && utf8.ValidString(s)
}

func (l *likeSegments) matchSegments(s string) bool {
	last := len(l.segs) - 1
	pos := l.prefix(s, 0, l.segs[0])
	if pos < 0 {
		return false
	}
	if last == 0 {
		return pos == len(s)
	}
	for _, seg := range l.segs[1:last] {
		if len(seg) == 0 {
			continue
		}
		pos = l.index(s, pos, seg)
		if pos < 0 {
			return false
		}
	}
	// 最後の区間は末尾に一致し、前の区間と重ならない。
	end := len(s)
	seg := l.segs[last]
	for k := len(seg) - 1; k >= 0; k-- {
		if end <= pos {
			return false
		}
		r, n := utf8.DecodeLastRuneInString(s[:end])
		var w int32
		if r < utf8.RuneSelf && n == 1 {
			w = l.ascii[r]
		} else {
			w = l.sorter(r)
		}
		if w != seg[k] {
			return false
		}
		end -= n
	}
	return true
}
