// Copyright 2023 Dolthub, Inc.
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

// Modified by Tsukinowa Inc.: SUPER のない KILL を自分のユーザーの接続に限って通し、ない接続を 1094、ほかのユーザーの接続を 1095 にする（docs/engine.md）

package planbuilder

import (
	"github.com/dolthub/vitess/go/mysql"
	ast "github.com/dolthub/vitess/go/vt/sqlparser"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/expression"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

func (b *Builder) buildKill(inScope *scope, kill *ast.Kill) (outScope *scope) {
	authErr := b.cat.AuthorizationHandler().HandleAuth(b.ctx, b.authQueryState, kill.Auth)
	outScope = inScope.push()
	connID64 := b.getInt64Value(inScope, kill.ConnID, "Error parsing KILL, expected int literal")
	connID32 := uint32(connID64)
	if int64(connID32) != connID64 {
		err := sql.ErrUnsupportedFeature.New("int literal is not unsigned 32-bit.")
		b.handleErr(err)
	}
	// HeimDB: MySQL と同じく、ない接続は 1094。SUPER がなくても自分のユーザーの接続は止められ、ほかのユーザーの接続は
	// 1095（ユーザー名で比べる。information_schema.processlist と同じ）。
	if b.ctx.ProcessList != nil {
		var found bool
		for _, p := range b.ctx.ProcessList.Processes() {
			if p.Connection != connID32 {
				continue
			}
			found = true
			if authErr != nil && b.authEnabled && p.User != b.ctx.Session.Client().User {
				b.handleErr(mysql.NewSQLError(mysql.ERKillDenied, mysql.SSUnknownSQLState, "You are not owner of thread %d", connID32))
			}
		}
		if !found {
			b.handleErr(mysql.NewSQLError(mysql.ERNoSuchThread, mysql.SSUnknownSQLState, "Unknown thread id: %d", connID32))
		}
	} else if authErr != nil && b.authEnabled {
		b.handleErr(authErr)
	}
	if kill.Connection {
		outScope.node = plan.NewKill(plan.KillType_Connection, connID32)
	} else {
		outScope.node = plan.NewKill(plan.KillType_Query, connID32)
	}
	return outScope
}

// getInt64Value returns the int64 literal value in the expression given, or an error with the errStr given if it
// cannot.
func (b *Builder) getInt64Value(inScope *scope, expr ast.Expr, errStr string) int64 {
	ie := b.getInt64Literal(inScope, expr, errStr)

	i, err := ie.Eval(b.ctx, nil)
	if err != nil {
		b.handleErr(err)
	}

	return i.(int64)
}

// getInt64Literal returns an int64 *expression.Literal for the value given, or an unsupported error with the string
// given if the expression doesn't represent an integer literal.
func (b *Builder) getInt64Literal(inScope *scope, expr ast.Expr, errStr string) *expression.Literal {
	e := b.buildScalar(inScope, expr)

	switch e := e.(type) {
	case *expression.Literal:
		if !types.IsInteger(e.Type()) {
			err := sql.ErrUnsupportedFeature.New(errStr)
			b.handleErr(err)
		}
	}
	nl, ok := e.(*expression.Literal)
	if !ok || !types.IsInteger(nl.Type()) {
		err := sql.ErrUnsupportedFeature.New(errStr)
		b.handleErr(err)
	} else {
		i64, _, err := types.Int64.Convert(b.ctx, nl.Value())
		if err != nil {
			err := sql.ErrUnsupportedFeature.New(errStr)
			b.handleErr(err)
		}
		return expression.NewLiteral(i64, types.Int64)
	}

	return nl
}
