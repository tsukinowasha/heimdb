// Copyright 2026 Tsukinowa Inc.
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

package analyzer

import (
	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/transform"
)

// setGlobalPrivilegeId は、SET の全体の値の権限を確かめるルールの ID（HeimDB が足した。rule_ids.go の連番とは別）。
const setGlobalPrivilegeId RuleId = 9000

// MySQL のエラー 1227（ER_SPECIFIC_ACCESS_DENIED_ERROR）と 3630（ER_PERSIST_ONLY_ACCESS_DENIED_ERROR）。
var (
	errSetGlobalDenied = mysql.NewSQLError(1227, "42000",
		"Access denied; you need (at least one of) the SUPER or SYSTEM_VARIABLES_ADMIN privilege(s) for this operation")
	errPersistOnlyDenied = mysql.NewSQLError(3630, "42000",
		"Access denied; you need SYSTEM_VARIABLES_ADMIN and PERSIST_RO_VARIABLES_ADMIN privileges for this operation")
)

// setGlobalPrivilege は、SET の全体の値（GLOBAL、PERSIST、PERSIST_ONLY）を、SUPER 権限のないユーザーならエラーにする
// （HeimDB の docs/plan.md の段階 35 と 42）。SET GLOBAL autocommit = 0 や sql_select_limit = 1 は、その後のすべての
// 接続の既定になり、書き込みが黙って失われたり、SELECT が 1 行しか返さなくなったりする。ない名前と読み取り専用の値は、
// MySQL と同じく権限より先にそのエラーにするため、ここでは通す。解析の前の段で動くので、PREPARE は通り、EXECUTE で
// 止まる（MySQL と同じ）。認証が無効なら確かめない。
func setGlobalPrivilege(ctx *sql.Context, a *Analyzer, n sql.Node, _ *plan.Scope, _ RuleSelector, _ *sql.QueryFlags) (sql.Node, transform.TreeIdentity, error) {
	var err error
	transform.Inspect(n, func(node sql.Node) bool {
		s, ok := node.(*plan.Set)
		if !ok || err != nil {
			return err == nil
		}
		for _, e := range s.Exprs {
			sf, ok := e.(*expression.SetField)
			if !ok {
				continue
			}
			sv, ok := sf.LeftChild.(*expression.SystemVar)
			if !ok {
				continue
			}
			scope, ok := sv.Scope.(*sql.MysqlScope)
			if !ok {
				continue
			}
			var denied error
			switch scope.Type {
			case sql.SystemVariableScope_Global, sql.SystemVariableScope_Persist:
				denied = errSetGlobalDenied
			case sql.SystemVariableScope_PersistOnly:
				denied = errPersistOnlyDenied
			default:
				continue
			}
			if v, _, ok := sql.SystemVariables.GetGlobal(sv.Name); !ok || v.IsReadOnly() {
				continue
			}
			if !a.Catalog.MySQLDb.UserHasPrivileges(ctx, sql.NewPrivilegedOperation(sql.PrivilegeCheckSubject{}, sql.PrivilegeType_Super)) {
				err = denied
				return false
			}
		}
		return true
	})
	return n, transform.SameTree, err
}
