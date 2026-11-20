package backend

import (
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// unreadVariables は、MySQL の互換のために SET を受け付けるが、HeimDB が読まない変数と、代わりに使うもの
// （docs/plan.md の段階 82）。プラグインが接続のたびに設定するので、エラーにはせず、変数ごとに 1 回、警告をログに出す
// （効いていると思い込まないように）。一覧は docs/operations.md の「読まない変数」。
var unreadVariables = map[string]string{
	"wait_timeout":             "HeimDB does not close idle connections; a connection idle in a transaction with the write lock is closed after -idle-lock-timeout",
	"interactive_timeout":      "HeimDB does not close idle connections; a connection idle in a transaction with the write lock is closed after -idle-lock-timeout",
	"innodb_lock_wait_timeout": "the write lock wait is -lock-wait-timeout",
	"lock_wait_timeout":        "the write lock wait is -lock-wait-timeout",
	"net_read_timeout":         "HeimDB does not read it",
	"net_write_timeout":        "HeimDB does not read it",
	"long_query_time":          "only the global value is read (SET GLOBAL long_query_time, by an administrator)",
}

var unreadWarned sync.Map

// SetSessionVariable は sql.Session の実装。HeimDB が読まない変数なら、変数ごとに 1 回、警告を出してから受け付ける。
func (s *Session) SetSessionVariable(ctx *sql.Context, name string, value interface{}) error {
	if note, ok := unreadVariables[strings.ToLower(name)]; ok {
		if _, warned := unreadWarned.LoadOrStore(strings.ToLower(name), true); !warned {
			logrus.Warnf("HeimDB: SET %s is accepted but has no effect: %s", strings.ToLower(name), note)
		}
	}
	return s.BaseSession.SetSessionVariable(ctx, name, value)
}
