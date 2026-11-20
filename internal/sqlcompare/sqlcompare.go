// Package sqlcompare は、本物の MySQL と HeimDB に同じ SQL を流して結果を比べるための共通処理を提供する。
package sqlcompare

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Run は 1 本の接続で q を実行し、結果を比較用の文字列にする。SELECT、SHOW、WITH は行を、それ以外は影響行数と
// LAST_INSERT_ID を、エラーは MySQL のエラー番号を返す。
func Run(ctx context.Context, conn *sql.Conn, q string) string {
	upper := strings.ToUpper(strings.TrimSpace(q))
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "SHOW") && !strings.HasPrefix(upper, "WITH") {
		res, err := conn.ExecContext(ctx, q)
		if err != nil {
			return errString(err)
		}
		affected, _ := res.RowsAffected()
		id, _ := res.LastInsertId()
		if strings.HasPrefix(upper, "ALTER ") {
			// MySQL は、テーブルを書き直す ALTER の影響行数に行数を返す（go-mysql-server は 0）。ストレージと無関係なので比べない。
			return "ok"
		}
		if strings.HasPrefix(upper, "SET ") {
			// go-mysql-server は SET に影響行数 1 を返す（MySQL は 0）。ストレージと無関係で、WordPress も見ないので比べない。
			return "ok"
		}
		return fmt.Sprintf("ok affected=%d insert_id=%d", affected, id)
	}
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return errString(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	// SHOW INDEX は、実装で値が違う列（Cardinality など）を除き、定義に関わる列だけを比べる。
	keep := func(string) bool { return true }
	if strings.HasPrefix(upper, "SHOW INDEX") {
		keep = func(c string) bool {
			switch c {
			case "Non_unique", "Key_name", "Seq_in_index", "Column_name", "Sub_part", "Null", "Index_type":
				return true
			}
			return false
		}
	}
	var b strings.Builder
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return errString(err)
		}
		var parts []string
		for i, v := range vals {
			if !keep(cols[i]) {
				continue
			}
			if v.Valid {
				parts = append(parts, v.String)
			} else {
				parts = append(parts, "NULL")
			}
		}
		b.WriteString(strings.Join(parts, " | ") + "\n")
	}
	if err := rows.Err(); err != nil {
		return errString(err)
	}
	return b.String()
}

// errString はエラーを MySQL のエラー番号で表す。文言は実装で違うので比べない。
func errString(err error) string {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Sprintf("error %d", me.Number)
	}
	return "error " + err.Error()
}
