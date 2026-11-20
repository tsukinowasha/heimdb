// Copyright 2020-2024 Dolthub, Inc.
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
// Modified by Tsukinowa Inc.: 複数の文の接続で、構文解析のエラーを見る（docs/engine.md）
// Modified by Tsukinowa Inc.: 文のエラーをセッションが置き換えられるようにする（QueryErrorTranslator。docs/engine.md）
// Modified by Tsukinowa Inc.: 文の処理の panic を回復してエラーにし、接続を保つ（recoverQueryPanic。docs/engine.md）
// Modified by Tsukinowa Inc.: OK と空の結果の文が途中でエラーになったら、文の計画のキャッシュを片付ける（docs/engine.md）
// Modified by Tsukinowa Inc.: 複数の文の接続で、エラーの文の後の文を実行しない（docs/engine.md）
// Modified by Tsukinowa Inc.: 切れた接続を、アカウントの同時接続数の上限の数から戻す（docs/engine.md）
// Modified by Tsukinowa Inc.: 失敗した文のエラーを、SHOW WARNINGS に Error の行として出す（docs/engine.md）
// Modified by Tsukinowa Inc.: バイナリプロトコルの文の印を付ける（WithBinaryProtocol。段階 116 の 3 度目のレビュー。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT と DOUBLE の列の情報の Decimals を MySQL と同じ 31 にする（段階 118。docs/engine.md）
// Modified by Tsukinowa Inc.: FLOAT(M,D) と DOUBLE(M,D) の列の情報の Decimals を D にし、FLOAT と DOUBLE の UNSIGNED に印を付ける（段階 120。docs/engine.md）
// Modified by Tsukinowa Inc.: 行を文のゴルーチンで読んで送り、切れた接続の見張りを 5 秒の後に始める（段階 124。docs/engine.md）
// Modified by Tsukinowa Inc.: 開いた接続と文を処理している接続を数え、文を始めるときに眠った Go のスレッドを起こす（段階 128。docs/engine.md）

package server

import (
	"context"
	"encoding/base64"
	goerrors "errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"runtime/debug"
	"runtime/trace"
	"sync/atomic"
	"time"

	"github.com/dolthub/vitess/go/mysql"
	"github.com/dolthub/vitess/go/netutil"
	"github.com/dolthub/vitess/go/sqltypes"
	querypb "github.com/dolthub/vitess/go/vt/proto/query"
	"github.com/dolthub/vitess/go/vt/sqlparser"
	"github.com/go-kit/kit/metrics/discard"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	otel "go.opentelemetry.io/otel/trace"
	"gopkg.in/src-d/go-errors.v1"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/internal/sockstate"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/analyzer"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/iters"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/plan"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/rowexec"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/types"
)

// ErrRowTimeout will be returned if the wait for the row is longer than the connection timeout
var ErrRowTimeout = errors.NewKind("row read wait bigger than connection timeout")

// ErrConnectionWasClosed will be returned if we try to use a previously closed connection
var ErrConnectionWasClosed = errors.NewKind("connection was closed")

// set this to true to get verbose error logging on every query (print a stack trace for most errors)
var verboseErrorLogging = false

const rowsBatch = 128

var tcpCheckerSleepDuration time.Duration = 5 * time.Second

type MultiStmtMode int

const (
	MultiStmtModeOff MultiStmtMode = 0
	MultiStmtModeOn  MultiStmtMode = 1
)

// Handler is a connection handler for a SQLe engine, implementing the Vitess mysql.Handler interface.
type Handler struct {
	e                 *sqle.Engine
	sm                *SessionManager
	readTimeout       time.Duration
	disableMultiStmts bool
	maxLoggedQueryLen int
	encodeLoggedQuery bool
	sel               ServerEventListener
	// HeimDB: 開いた接続と、文を処理している接続の数（段階 128。netpoll_heimdb.go）
	openConns atomic.Int64
	busyConns atomic.Int64
}

var _ mysql.Handler = (*Handler)(nil)
var _ mysql.ExtendedHandler = (*Handler)(nil)
var _ mysql.BinlogReplicaHandler = (*Handler)(nil)

// NewConnection reports that a new connection has been established.
func (h *Handler) NewConnection(c *mysql.Conn) {
	if h.sel != nil {
		h.sel.ClientConnected()
	}

	h.sm.AddConn(c)
	h.openConns.Add(1) // HeimDB: 段階 128
	updateMaxUsedConnectionsStatusVariable()
	sql.StatusVariables.IncrementGlobal("Connections", 1)

	c.DisableClientMultiStatements = h.disableMultiStmts
	logrus.WithField(sql.ConnectionIdLogField, c.ConnectionID).WithField("DisableClientMultiStatements", c.DisableClientMultiStatements).Infof("NewConnection")
}

func (h *Handler) ConnectionAborted(_ *mysql.Conn, _ string) error {
	sql.StatusVariables.IncrementGlobal("Aborted_connects", 1)
	return nil
}

func (h *Handler) ComInitDB(c *mysql.Conn, schemaName string) error {
	// SetDB itself handles session and processlist operation lifecycle callbacks.
	err := h.sm.SetDB(context.Background(), c, schemaName)
	if err != nil {
		logrus.WithField("database", schemaName).Errorf("unable to process ComInitDB: %s", err.Error())
		err = sql.CastSQLError(err)
	}
	return err
}

// ComPrepare parses, partially analyzes, and caches a prepared statement's plan
// with the given [c.ConnectionID].
func (h *Handler) ComPrepare(ctx context.Context, c *mysql.Conn, query string, prepare *mysql.PrepareData) (_ []*querypb.Field, err error) {
	logrus.WithField("query", query).
		WithField("paramsCount", prepare.ParamsCount).
		WithField("statementId", prepare.StatementID).Debugf("preparing query")

	sqlCtx, err := h.sm.NewContextWithQuery(ctx, c, query)
	if err != nil {
		return nil, err
	}
	sqlCtx, err = sqlCtx.ProcessList.BeginOperation(sqlCtx)
	if err != nil {
		return nil, err
	}
	defer sqlCtx.ProcessList.EndOperation(sqlCtx)
	err = sql.SessionCommandBegin(sqlCtx.Session)
	if err != nil {
		return nil, err
	}
	defer sql.SessionCommandEnd(sqlCtx.Session)
	defer recoverQueryPanic(sqlCtx, &err)

	var analyzed sql.Node
	if analyzer.PreparedStmtDisabled {
		analyzed, err = h.e.AnalyzeQuery(sqlCtx, query)
	} else {
		analyzed, err = h.e.PrepareQuery(sqlCtx, query)
	}
	if err != nil {
		logrus.WithField("query", query).Errorf("unable to prepare query: %s", err.Error())
		err := sql.CastSQLError(err)
		return nil, err
	}

	// A nil result signals to the handler that the query is not a SELECT statement.
	if nodeReturnsOkResultSchema(analyzed) || types.IsOkResultSchema(analyzed.Schema()) {
		return nil, nil
	}

	return schemaToFields(sqlCtx, analyzed.Schema()), nil
}

// These nodes will eventually return an OK result, but their intermediate forms here return a different schema
// than they will at execution time.
func nodeReturnsOkResultSchema(node sql.Node) bool {
	switch node.(type) {
	case *plan.InsertInto, *plan.Update, *plan.UpdateJoin, *plan.DeleteFrom:
		return true
	}
	return false
}

func (h *Handler) ComPrepareParsed(ctx context.Context, c *mysql.Conn, query string, parsed sqlparser.Statement, prepare *mysql.PrepareData) (_ mysql.ParsedQuery, _ []*querypb.Field, err error) {
	logrus.WithField("query", query).
		WithField("paramsCount", prepare.ParamsCount).
		WithField("statementId", prepare.StatementID).Debugf("preparing query")

	sqlCtx, err := h.sm.NewContextWithQuery(ctx, c, query)
	if err != nil {
		return nil, nil, err
	}
	sqlCtx, err = sqlCtx.ProcessList.BeginOperation(sqlCtx)
	if err != nil {
		return nil, nil, err
	}
	defer sqlCtx.ProcessList.EndOperation(sqlCtx)
	err = sql.SessionCommandBegin(sqlCtx.Session)
	if err != nil {
		return nil, nil, err
	}
	defer sql.SessionCommandEnd(sqlCtx.Session)
	defer recoverQueryPanic(sqlCtx, &err)

	analyzed, err := h.e.PrepareParsedQuery(sqlCtx, query, query, parsed)
	if err != nil {
		logrus.WithField("query", query).Errorf("unable to prepare query: %s", err.Error())
		err := sql.CastSQLError(err)
		return nil, nil, err
	}

	var fields []*querypb.Field
	// The return result fields should only be directly translated if it doesn't correspond to an OK result.
	// See comment in ComPrepare
	if !(nodeReturnsOkResultSchema(analyzed) || types.IsOkResultSchema(analyzed.Schema())) {
		fields = nil
	} else {
		fields = schemaToFields(sqlCtx, analyzed.Schema())
	}

	return analyzed, fields, nil
}

func (h *Handler) ComBind(ctx context.Context, c *mysql.Conn, query string, parsedQuery mysql.ParsedQuery, prepare *mysql.PrepareData) (_ mysql.BoundQuery, _ []*querypb.Field, err error) {
	sqlCtx, err := h.sm.NewContextWithQuery(ctx, c, query)
	if err != nil {
		return nil, nil, err
	}
	sqlCtx, err = sqlCtx.ProcessList.BeginOperation(sqlCtx)
	if err != nil {
		return nil, nil, err
	}
	defer sqlCtx.ProcessList.EndOperation(sqlCtx)
	err = sql.SessionCommandBegin(sqlCtx.Session)
	if err != nil {
		return nil, nil, err
	}
	defer sql.SessionCommandEnd(sqlCtx.Session)
	defer recoverQueryPanic(sqlCtx, &err)

	stmt, ok := parsedQuery.(sqlparser.Statement)
	if !ok {
		return nil, nil, fmt.Errorf("parsedQuery must be a sqlparser.Statement, but got %T", parsedQuery)
	}

	bindingExprs, err := bindingsToExprs(prepare.BindVars)
	if err != nil {
		return nil, nil, err
	}

	queryPlan, err := h.e.BoundQueryPlan(sqlCtx, query, stmt, bindingExprs)
	if err != nil {
		return nil, nil, err
	}

	return queryPlan, schemaToFields(sqlCtx, queryPlan.Schema()), nil
}

func (h *Handler) ComExecuteBound(ctx context.Context, conn *mysql.Conn, query string, boundQuery mysql.BoundQuery, callback mysql.ResultSpoolFn) error {
	plan, ok := boundQuery.(sql.Node)
	if !ok {
		return fmt.Errorf("boundQuery must be a sql.Node, but got %T", boundQuery)
	}

	return h.errorWrappedComExec(ctx, conn, query, plan, callback)
}

func (h *Handler) ComStmtExecute(ctx context.Context, c *mysql.Conn, prepare *mysql.PrepareData, callback func(*sqltypes.Result) error) error {
	// HeimDB: 結果の行はバイナリプロトコルで送る（DOUBLE の無限大を値のまま送る。段階 116 の 3 度目のレビュー）
	ctx = sql.WithBinaryProtocol(ctx)
	_, err := h.errorWrappedDoQuery(ctx, c, prepare.PrepareStmt, nil, MultiStmtModeOff, prepare.BindVars, func(res *sqltypes.Result, more bool) error {
		return callback(res)
	}, &sql.QueryFlags{})
	return err
}

// ComResetConnection implements the mysql.Handler interface.
//
// This command resets the connection's session, clearing out any cached prepared statements, locks, user and
// session variables. The currently selected database is preserved.
//
// The COM_RESET command can be sent manually through the mysql client by issuing the "resetconnection" (or "\x")
// client command.
func (h *Handler) ComResetConnection(c *mysql.Conn) error {
	logrus.WithField("connectionId", c.ConnectionID).Debug("COM_RESET_CONNECTION command received")

	// Grab the currently selected database name
	db := h.sm.GetCurrentDB(c)

	// Dispose of the connection's current session
	h.maybeReleaseAllLocks(c)
	h.e.CloseSession(c.ConnectionID)

	ctx := context.Background()

	// Create a new session and set the current database
	err := h.sm.NewSession(ctx, c)
	if err != nil {
		return err
	}

	return h.sm.SetDB(ctx, c, db)
}

func (h *Handler) ParserOptionsForConnection(c *mysql.Conn) (sqlparser.ParserOptions, error) {
	ctx, err := h.sm.NewContextWithQuery(context.Background(), c, "")
	if err != nil {
		return sqlparser.ParserOptions{}, err
	}
	return sql.LoadSqlMode(ctx).ParserOptions(), nil
}

// ConnectionClosed reports that a connection has been closed.
func (h *Handler) ConnectionClosed(c *mysql.Conn) {
	defer func() {
		if h.sel != nil {
			h.sel.ClientDisconnected()
		}
	}()

	defer h.sm.RemoveConn(c)
	defer h.e.CloseSession(c.ConnectionID)
	defer h.openConns.Add(-1) // HeimDB: 段階 128
	// HeimDB: アカウントの同時接続数の上限で数えた接続を戻す（sql/mysql_db/connlimit.go）。
	if db := h.e.Analyzer.Catalog.MySQLDb; db != nil {
		db.ConnectionClosed(c.ConnectionID)
	}

	h.maybeReleaseAllLocks(c)

	logrus.WithField(sql.ConnectionIdLogField, c.ConnectionID).Infof("ConnectionClosed")
}

// maybeReleaseAllLocks makes a best effort attempt to release all locks on the given connection. If the attempt fails,
// an error is logged but not returned.
func (h *Handler) maybeReleaseAllLocks(c *mysql.Conn) {
	if ctx, err := h.sm.NewContextWithQuery(context.Background(), c, ""); err != nil {
		logrus.Errorf("unable to release all locks on session close: %s", err)
		logrus.Errorf("unable to unlock tables on session close: %s", err)
	} else {
		_, err = h.e.LS.ReleaseAll(ctx)
		if err != nil {
			logrus.Errorf("unable to release all locks on session close: %s", err)
		}
		if err = h.e.Analyzer.Catalog.UnlockTables(ctx, c.ConnectionID); err != nil {
			logrus.Errorf("unable to unlock tables on session close: %s", err)
		}
	}
}

func (h *Handler) ComMultiQuery(
	ctx context.Context,
	c *mysql.Conn,
	query string,
	callback mysql.ResultSpoolFn,
) (string, error) {
	return h.errorWrappedDoQuery(ctx, c, query, nil, MultiStmtModeOn, nil, callback, &sql.QueryFlags{})
}

// ComQuery executes a SQL query on the SQLe engine.
func (h *Handler) ComQuery(
	ctx context.Context,
	c *mysql.Conn,
	query string,
	callback mysql.ResultSpoolFn,
) error {
	_, err := h.errorWrappedDoQuery(ctx, c, query, nil, MultiStmtModeOff, nil, callback, &sql.QueryFlags{})
	return err
}

// ComParsedQuery executes a pre-parsed SQL query on the SQLe engine.
func (h *Handler) ComParsedQuery(
	ctx context.Context,
	c *mysql.Conn,
	query string,
	parsed sqlparser.Statement,
	callback mysql.ResultSpoolFn,
) error {
	_, err := h.errorWrappedDoQuery(ctx, c, query, parsed, MultiStmtModeOff, nil, callback, &sql.QueryFlags{})
	return err
}

// ComRegisterReplica implements the mysql.BinlogReplicaHandler interface.
func (h *Handler) ComRegisterReplica(c *mysql.Conn, replicaHost string, replicaPort uint16, replicaUser string, replicaPassword string) error {
	// TODO: replicaUser and replicaPassword should be validated at this layer (GMS);
	//       confirm if that has been done already (doesn't seem likely)

	logrus.StandardLogger().
		WithField("connectionId", c.ConnectionID).
		WithField("replicaHost", replicaHost).
		WithField("replicaPort", replicaPort).
		Debug("Handling COM_REGISTER_REPLICA")

	if !h.e.Analyzer.Catalog.HasBinlogPrimaryController() {
		return nil
	}

	newCtx := sql.NewContext(context.Background())
	primaryController := h.e.Analyzer.Catalog.GetBinlogPrimaryController()
	return primaryController.RegisterReplica(newCtx, c, replicaHost, replicaPort)
}

// ComBinlogDumpGTID implements the mysql.BinlogReplicaHandler interface.
func (h *Handler) ComBinlogDumpGTID(c *mysql.Conn, logFile string, logPos uint64, gtidSet mysql.GTIDSet) error {
	if !h.e.Analyzer.Catalog.HasBinlogPrimaryController() {
		return nil
	}

	logrus.StandardLogger().
		WithField("connectionId", c.ConnectionID).
		Debug("Handling COM_BINLOG_DUMP_GTID")

	// TODO: is logfile and logpos ever actually needed for COM_BINLOG_DUMP_GTID?
	newCtx := sql.NewContext(context.Background())
	primaryController := h.e.Analyzer.Catalog.GetBinlogPrimaryController()
	return primaryController.BinlogDumpGtid(newCtx, c, gtidSet)
}

var queryLoggingRegex = regexp.MustCompile(`[\r\n\t ]+`)

// QueryPanicHandler は、文の処理の panic を知らされるセッション（HeimDB は、その文の書き込みを取り消す）。
type QueryPanicHandler interface {
	QueryPanicked()
}

// QueryPanicError は、文の処理の panic を回復したエラー（エラー 1105）。HeimDB: もとは vitess が回復して接続を閉じるか、
// 行を送る処理では「handler caught panic」のエラーにしていた。
type QueryPanicError struct {
	Value any
}

func (e *QueryPanicError) Error() string {
	if err, ok := e.Value.(error); ok {
		return "HeimDB: internal error: " + err.Error()
	}
	return fmt.Sprintf("HeimDB: internal error: %v", e.Value)
}

// newQueryPanicError は、回復した panic をログ（スタック付き。文は出さない。パスワードを含みうる）に出し、エラーにする。
func newQueryPanicError(ctx *sql.Context, r any) error {
	logrus.WithField("connectionID", ctx.Session.ID()).Errorf("HeimDB: recovered from a panic in a statement: %v\n%s", r, debug.Stack())
	return &QueryPanicError{Value: r}
}

// recoverQueryPanic は、文の処理（構文解析、解析、実行、準備した文）の panic を回復してエラーにし、panic の
// エラー（行を送る処理で回復したものを含む）ならセッションに知らせる。defer で直接呼ぶ（recover はそうでないと
// 効かない）。CommandEnd より後に defer し、先に動かす。
func recoverQueryPanic(ctx *sql.Context, err *error) {
	if r := recover(); r != nil {
		*err = newQueryPanicError(ctx, r)
	}
	var pe *QueryPanicError
	if *err != nil && goerrors.As(*err, &pe) {
		if h, ok := ctx.Session.(QueryPanicHandler); ok {
			h.QueryPanicked()
		}
	}
}

// statementWarnings は、文の始めに警告を消したかを覚えるセッション（sql.BaseSession。HeimDB）。
type statementWarnings interface {
	BeginStatementWarnings()
	WarningsCleared() bool
}

// QueryErrorTranslator は、文のエラーを置き換えるセッション（HeimDB）。
type QueryErrorTranslator interface {
	TranslateQueryError(err error) error
}

func (h *Handler) doQuery(
	ctx context.Context,
	c *mysql.Conn,
	query string,
	parsed sqlparser.Statement,
	analyzedPlan sql.Node,
	mode MultiStmtMode,
	queryExec QueryExecutor,
	bindings map[string]*querypb.BindVariable,
	callback func(*sqltypes.Result, bool) error,
	qFlags *sql.QueryFlags,
) (remainder string, err error) {
	var sqlCtx *sql.Context
	sqlCtx, err = h.sm.NewContextWithQuery(ctx, c, query)
	if err != nil {
		return "", err
	}
	// TODO: it would be nice to put this logic in the engine, not the handler, but we don't want the process to be
	//  marked done until we're done spooling rows over the wire
	sqlCtx, err = sqlCtx.ProcessList.BeginQuery(sqlCtx, query)
	if err != nil {
		return remainder, err
	}
	defer sqlCtx.ProcessList.EndQuery(sqlCtx)
	err = sql.SessionCommandBegin(sqlCtx.Session)
	if err != nil {
		return "", err
	}
	defer sql.SessionCommandEnd(sqlCtx.Session)
	// HeimDB: 失敗した文のエラーを、MySQL と同じく SHOW WARNINGS に Error の行として出す。文の警告を消す前に
	// 失敗した（構文解析や計画のエラー）ときは、前の文の警告を消してから足す。失敗する前に出た警告は残す。
	// 置き換えたエラー（下の defer が先に動く）を、接続先に返すのと同じ番号と文で書く。
	if sw, ok := sqlCtx.Session.(statementWarnings); ok {
		sw.BeginStatementWarnings()
		defer func() {
			if err == nil {
				return
			}
			if !sw.WarningsCleared() {
				sqlCtx.ClearWarnings()
			}
			if se := sql.CastSQLError(err); se != nil {
				sqlCtx.Session.Warn(&sql.Warning{Level: "Error", Code: se.Num, Message: se.Message})
			}
		}()
	}
	// HeimDB: 文のエラーを、セッションが置き換える（実行時間の上限で止めた文を、取り消しのエラーではなく MySQL と
	// 同じエラー 3024 にする）。CommandEnd より先に呼ぶ（defer は逆順）。
	if t, ok := sqlCtx.Session.(QueryErrorTranslator); ok {
		defer func() {
			if err != nil {
				err = t.TranslateQueryError(err)
			}
		}()
	}
	// HeimDB: エラー（実行時のエラーと panic を含む）の文の後は、残りの文を実行しない（MySQL と同じ。もとは構文解析の
	// エラーのときだけ止めていた。docs/plan.md の段階 46）。
	defer func() {
		if err != nil {
			remainder = ""
		}
	}()
	// HeimDB: 文の計数（observeQuery）は、panic を回復した後のエラーで行う（finish は後で決まる）。
	var finish func(error)
	defer func() {
		if finish != nil {
			finish(err)
		}
	}()
	defer recoverQueryPanic(sqlCtx, &err)

	start := time.Now()

	var prequery string
	if parsed == nil {
		_, inPreparedCache := h.e.PreparedDataCache.GetCachedStmt(sqlCtx.Session.ID(), query)
		if mode == MultiStmtModeOn && !inPreparedCache {
			parsed, prequery, remainder, err = h.e.Parser.Parse(sqlCtx, query, true)
			// HeimDB: 構文解析のエラーを見る（もとは見ずに、返った文をそのまま実行し、後の queryExec が err を
			// 上書きしていた）。残りの文も実行しない（MySQL と同じ）。ただし、コメントや空白だけの文（ErrEmpty）は、
			// これまでどおり queryExec に任せる（planbuilder が何もしない文にする。docs/plan.md の段階 43）。
			if err != nil && !goerrors.Is(err, sqlparser.ErrEmpty) {
				return "", err
			}
			if prequery != "" {
				query = prequery
			}
		}
	}

	more := remainder != ""

	var queryStr string
	if h.encodeLoggedQuery {
		queryStr = base64.StdEncoding.EncodeToString([]byte(query))
	} else if logrus.IsLevelEnabled(logrus.DebugLevel) {
		// this is expensive, so skip this unless we're logging at DEBUG level
		queryStr = string(queryLoggingRegex.ReplaceAll([]byte(query), []byte(" ")))
		if h.maxLoggedQueryLen > 0 && len(queryStr) > h.maxLoggedQueryLen {
			queryStr = queryStr[:h.maxLoggedQueryLen] + "..."
		}
	}

	if queryStr != "" {
		sqlCtx.SetLogger(sqlCtx.GetLogger().WithField("query", queryStr))
	}
	sqlCtx.GetLogger().Debugf("Starting query")

	finish = observeQuery(sqlCtx, query)

	sqlCtx.GetLogger().Tracef("beginning execution")

	var schema sql.Schema
	var rowIter sql.RowIter
	qFlags.Set(sql.QFlagDeferProjections)
	schema, rowIter, qFlags, err = queryExec(sqlCtx, query, parsed, analyzedPlan, bindings, qFlags)
	if err != nil {
		sqlCtx.GetLogger().WithError(err).Warn("error running query")
		if verboseErrorLogging {
			fmt.Printf("Err: %+v", err)
		}
		return remainder, err
	}

	// create result before goroutines to avoid |ctx| racing
	resultFields := schemaToFields(sqlCtx, schema)
	var r *sqltypes.Result
	var processedAtLeastOneBatch bool

	buf := sql.ByteBufPool.Get().(*sql.ByteBuffer)
	defer func() {
		buf.Reset()
		sql.ByteBufPool.Put(buf)
	}()

	// zero/single return schema use spooling shortcut
	if types.IsOkResultSchema(schema) {
		r, err = resultForOkIter(sqlCtx, rowIter)
	} else if schema == nil {
		r, err = resultForEmptyIter(sqlCtx, rowIter, resultFields)
	} else if analyzer.FlagIsSet(qFlags, sql.QFlagMax1Row) {
		r, err = resultForMax1RowIter(sqlCtx, schema, rowIter, resultFields, buf)
	} else {
		r, processedAtLeastOneBatch, err = h.resultForDefaultIter(sqlCtx, c, schema, rowIter, callback, resultFields, more, buf)
	}
	if err != nil {
		return remainder, err
	}

	if err = setConnStatusFlags(sqlCtx, c); err != nil {
		return remainder, err
	}

	switch len(r.Rows) {
	case 0:
		if len(r.Info) > 0 {
			sqlCtx.GetLogger().Tracef("returning result %s", r.Info)
		} else {
			sqlCtx.GetLogger().Tracef("returning empty result")
		}
	case 1:
		sqlCtx.GetLogger().Tracef("returning result %v", r)
	}

	sqlCtx.GetLogger().Debugf("Query finished in %d ms", time.Since(start).Milliseconds())

	// processedAtLeastOneBatch means we already called callback() at least
	// once, so no need to call it if RowsAffected == 0.
	if r != nil && (r.RowsAffected == 0 && processedAtLeastOneBatch) {
		return remainder, nil
	}

	return remainder, callback(r, more)
}

// resultForOkIter reads a maximum of one result row from a result iterator.
func resultForOkIter(ctx *sql.Context, iter sql.RowIter) (*sqltypes.Result, error) {
	defer trace.StartRegion(ctx, "Handler.resultForOkIter").End()

	row, err := iter.Next(ctx)
	if err != nil {
		disposeOnError(iter)
		return nil, err
	}
	_, err = iter.Next(ctx)
	if err != io.EOF {
		disposeOnError(iter)
		return nil, fmt.Errorf("result schema iterator returned more than one row")
	}
	if err := iter.Close(ctx); err != nil {
		return nil, err
	}
	return resultFromOkResult(row[0].(types.OkResult)), nil
}

// disposeOnError は、エラーで終わった文の計画が持つキャッシュ（サブクエリなど）を MemoryManager から外す。HeimDB:
// 閉じない（Close は自動コミットのコミットを走らせ、途中で止まった文の書き込みを残す）ので、文の終わりに
// TrackedRowIter が閉じるときに走る片付けだけをする。行の iterator のキャッシュは、書き込みの iterator が閉じている。
func disposeOnError(iter sql.RowIter) {
	if c, ok := iter.(*rowexec.ExprCloserIter); ok {
		iter = c.GetIter()
	}
	if d, ok := iter.(sql.Disposable); ok {
		d.Dispose()
	}
}

// resultForEmptyIter ensures that an expected empty iterator returns no rows.
func resultForEmptyIter(ctx *sql.Context, iter sql.RowIter, resultFields []*querypb.Field) (*sqltypes.Result, error) {
	defer trace.StartRegion(ctx, "Handler.resultForEmptyIter").End()
	if _, err := iter.Next(ctx); err != io.EOF {
		disposeOnError(iter)
		return nil, fmt.Errorf("result schema iterator returned more than zero rows")
	}
	if err := iter.Close(ctx); err != nil {
		return nil, err
	}
	return &sqltypes.Result{Fields: resultFields}, nil
}

// GetDeferredProjections looks for a top-level deferred projection, retrieves its projections, and removes it from the
// iterator tree.
func GetDeferredProjections(iter sql.RowIter) (sql.RowIter, []sql.Expression) {
	switch i := iter.(type) {
	case *rowexec.ExprCloserIter:
		if newChild, projs := GetDeferredProjections(i.GetIter()); projs != nil {
			return i.WithChildIter(newChild), projs
		}
	case *plan.TrackedRowIter:
		if newChild, projs := GetDeferredProjections(i.GetIter()); projs != nil {
			return i.WithChildIter(newChild), projs
		}
	case *rowexec.TransactionCommittingIter:
		if newChild, projs := GetDeferredProjections(i.GetIter()); projs != nil {
			return i.WithChildIter(newChild), projs
		}
	case *iters.LimitIter:
		if newChild, projs := GetDeferredProjections(i.ChildIter); projs != nil {
			i.ChildIter = newChild
			return i, projs
		}
	case *rowexec.ProjectIter:
		if i.CanDefer() {
			return i.GetChildIter(), i.GetProjections()
		}
	}
	return iter, nil
}

// resultForMax1RowIter ensures that an empty iterator returns at most one row
func resultForMax1RowIter(ctx *sql.Context, schema sql.Schema, iter sql.RowIter, resultFields []*querypb.Field, buf *sql.ByteBuffer) (*sqltypes.Result, error) {
	defer trace.StartRegion(ctx, "Handler.resultForMax1RowIter").End()

	defer iter.Close(ctx)

	row, err := iter.Next(ctx)
	if err == io.EOF {
		return &sqltypes.Result{Fields: resultFields}, nil
	} else if err != nil {
		return nil, err
	}

	if _, err = iter.Next(ctx); err != io.EOF {
		return nil, fmt.Errorf("result max1Row iterator returned more than one row")
	}

	outputRow, err := RowToSQL(ctx, schema, row, nil, buf)
	if err != nil {
		return nil, err
	}

	ctx.GetLogger().Tracef("spooling result row %s", outputRow)

	return &sqltypes.Result{Fields: resultFields, Rows: [][]sqltypes.Value{outputRow}, RowsAffected: 1}, nil
}

// resultForDefaultIter reads batches of rows from the iterator
// and writes results into the callback function.
//
// HeimDB: 行は、文を受けたゴルーチンで読み、変換して送る（段階 124。もとは文ごとに errgroup の 4 つのゴルーチンと
// 512 行のチャンネル、タイマーを作り、WordPress の軽い文ではその起動と切り替えが CPU の 1 割ほどを使っていた）。
// 次のふるまいは、もとと同じにする。
//   - panic は、ほかのエラーと同じく newQueryPanicError のエラーにする（行を読む、送る、閉じるのどこでも）。
//   - 行ごとに文の context を見て、取り消されていれば（KILL QUERY、実行時間の上限）その理由のエラーを返す。
//   - h.readTimeout が 0 でなければ、次の行を待つ時間がそれを超えると ErrRowTimeout を返す（止まった Next は、
//     time.AfterFunc が context を取り消して戻す。接続先へ送る間は数えない）。
//   - 切れた接続の見張り（pollForClosedConnection）は、文が tcpCheckerSleepDuration（5 秒）続いたときだけ始め、
//     接続が切れていれば文の context を ErrConnectionWasClosed で取り消す。文の終わりに止め、終わるのを待つ。
//   - iter.Close は、すべての行を送った後に呼ぶ（エラーと panic のときも）。
func (h *Handler) resultForDefaultIter(ctx *sql.Context, c *mysql.Conn, schema sql.Schema, iter sql.RowIter, callback func(*sqltypes.Result, bool) error, resultFields []*querypb.Field, more bool, buf *sql.ByteBuffer) (r *sqltypes.Result, processedAtLeastOneBatch bool, err error) {
	defer trace.StartRegion(ctx, "Handler.resultForDefaultIter").End()

	// The deferred functions run in reverse order: the read timeout and the closed-connection poller stop first,
	// then the iterator is closed, then the statement's context is released, and last a panic becomes an error and
	// the error is logged.
	defer func() {
		if recoveredPanic := recover(); recoveredPanic != nil {
			err = goerrors.Join(err, newQueryPanicError(ctx, recoveredPanic))
		}
		if err != nil {
			ctx.GetLogger().WithError(err).Warn("error running query")
			if verboseErrorLogging {
				fmt.Printf("Err: %+v", err)
			}
			r, processedAtLeastOneBatch = nil, false
		}
	}()

	tcpConn, isTCP := maybeGetTCPConn(c.Conn)
	var cancel context.CancelCauseFunc
	if isTCP || h.readTimeout > 0 {
		var cctx context.Context
		cctx, cancel = context.WithCancelCause(ctx.Context)
		ctx = ctx.WithContext(cctx)
		defer cancel(nil)
	}

	// Read rows off the row iterator.
	iter, projs := GetDeferredProjections(iter)
	defer func() {
		// Close() kills this PID in the process list, so it runs after all rows have been sent over the wire.
		if cerr := iter.Close(ctx); cerr != nil && err == nil {
			err = cerr
		}
	}()

	// The poller and the read timeout log from their own goroutines with the session's logger, taken here.
	logger := ctx.GetLogger()
	if isTCP {
		poll := h.startClosedConnectionPoll(logger, tcpConn, cancel)
		defer poll.stop()
	}

	// If there is a timeout, it is enforced to ensure that Vitess has a chance to call Handler.CloseConnection().
	// Without one, rows are waited for as long as the statement runs (as before, where the one-minute timer only
	// looped).
	var rowTimer *time.Timer
	waitTime := h.readTimeout
	if waitTime > 0 {
		rowTimer = time.AfterFunc(waitTime, func() {
			logger.Tracef("connection timeout")
			cancel(ErrRowTimeout.New())
		})
		defer rowTimer.Stop()
	}

	// Wrap the callback to include a BytesBuffer.Reset() for non-cursor requests, to
	// clean out rows that have already been spooled.
	resetCallback := func(r *sqltypes.Result, more bool) error {
		// A server-side cursor allows the caller to fetch results cached on the server-side,
		// so if a cursor exists, we can't release the buffer memory yet.
		if c.StatusFlags&uint16(mysql.ServerCursorExists) != 0 {
			defer buf.Reset()
		}
		return callback(r, more)
	}

	traceRows := logger.Logger.IsLevelEnabled(logrus.TraceLevel)
	for {
		if r == nil {
			r = &sqltypes.Result{Fields: resultFields}
		}
		if r.RowsAffected == rowsBatch {
			if rowTimer != nil {
				rowTimer.Stop() // the time spent sending rows is not a wait for a row
			}
			if err := resetCallback(r, more); err != nil {
				return nil, false, err
			}
			r = nil
			processedAtLeastOneBatch = true
			continue
		}

		if ctx.Err() != nil {
			return nil, false, context.Cause(ctx)
		}
		if rowTimer != nil {
			rowTimer.Reset(waitTime)
		}
		row, err := iter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				// The statement was cancelled (KILL QUERY, a statement timeout, the read timeout or a closed
				// connection) while the iterator ran: report why, as the row-sending goroutine did before.
				return nil, false, context.Cause(ctx)
			}
			return nil, false, err
		}
		if types.IsOkResult(row) {
			if len(r.Rows) > 0 {
				panic("Got OkResult mixed with RowResult")
			}
			r = resultFromOkResult(row[0].(types.OkResult))
			continue
		}

		outputRow, err := RowToSQL(ctx, schema, row, projs, buf)
		if err != nil {
			return nil, false, err
		}

		if traceRows {
			logger.Tracef("spooling result row %s", outputRow)
		}
		r.Rows = append(r.Rows, outputRow)
		r.RowsAffected++
	}
	return r, processedAtLeastOneBatch, nil
}

// See https://dev.mysql.com/doc/internals/en/status-flags.html
func setConnStatusFlags(ctx *sql.Context, c *mysql.Conn) error {
	ok, err := isSessionAutocommit(ctx)
	if err != nil {
		return err
	}
	if ok {
		c.StatusFlags |= uint16(mysql.ServerStatusAutocommit)
	} else {
		c.StatusFlags &= ^uint16(mysql.ServerStatusAutocommit)
	}

	if t := ctx.GetTransaction(); t != nil {
		c.StatusFlags |= uint16(mysql.ServerInTransaction)
	} else {
		c.StatusFlags &= ^uint16(mysql.ServerInTransaction)
	}

	return nil
}

func isSessionAutocommit(ctx *sql.Context) (bool, error) {
	autoCommitSessionVar, err := ctx.GetSessionVariable(ctx, sql.AutoCommitSessionVar)
	if err != nil {
		return false, err
	}
	return sql.ConvertToBool(ctx, autoCommitSessionVar)
}

// Call doQuery and cast known errors to SQLError
func (h *Handler) errorWrappedDoQuery(
	ctx context.Context,
	c *mysql.Conn,
	query string,
	parsed sqlparser.Statement,
	mode MultiStmtMode,
	bindings map[string]*querypb.BindVariable,
	callback func(*sqltypes.Result, bool) error,
	qFlags *sql.QueryFlags,
) (string, error) {
	start := time.Now()
	// HeimDB: ほかの接続の次の文を読むスレッドを残す（段階 128。netpoll_heimdb.go）
	h.commandStarted()
	defer h.commandDone()
	if h.sel != nil {
		h.sel.QueryStarted()
	}

	remainder, err := h.doQuery(ctx, c, query, parsed, nil, mode, h.executeQuery, bindings, callback, qFlags)
	if err != nil {
		err = sql.CastSQLError(err)
	}

	if h.sel != nil {
		h.sel.QueryCompleted(err == nil, time.Since(start))
	}

	return remainder, err
}

// Call doQuery and cast known errors to SQLError
func (h *Handler) errorWrappedComExec(
	ctx context.Context,
	c *mysql.Conn,
	query string,
	analyzedPlan sql.Node,
	callback func(*sqltypes.Result, bool) error,
) error {
	start := time.Now()
	// HeimDB: ほかの接続の次の文を読むスレッドを残す（段階 128。netpoll_heimdb.go）
	h.commandStarted()
	defer h.commandDone()
	if h.sel != nil {
		h.sel.QueryStarted()
	}

	_, err := h.doQuery(ctx, c, query, nil, analyzedPlan, MultiStmtModeOff, h.executeBoundPlan, nil, callback, nil)

	if err != nil {
		err = sql.CastSQLError(err)
	}

	if h.sel != nil {
		h.sel.QueryCompleted(err == nil, time.Since(start))
	}

	return err
}

// closedConnectionPoll は、文の途中で接続が切れたかを見張る（HeimDB: 段階 124）。文が tcpCheckerSleepDuration 続いた
// ときに始め（それより短い文では /proc を読まない）、stop で止めて終わるのを待つ。
type closedConnectionPoll struct {
	timer *time.Timer
	quit  chan struct{} // stop が閉じる
	done  chan struct{} // 見張りのゴルーチンが終わると閉じる
}

// Test hooks for the socket checks (HeimDB: stage 124).
var (
	getConnInode      = sockstate.GetConnInode
	getInodeSockState = sockstate.GetInodeSockState
)

// startClosedConnectionPoll は、tcpCheckerSleepDuration の後に pollForClosedConnection を始める。接続が切れていれば
// cancel を ErrConnectionWasClosed で呼ぶ。見張りの panic は回復して、文を QueryPanicError で止める（もとの errgroup
// の pan2err と同じく、プロセスを落とさない）。
func (h *Handler) startClosedConnectionPoll(logger *logrus.Entry, tcpConn *net.TCPConn, cancel context.CancelCauseFunc) *closedConnectionPoll {
	p := &closedConnectionPoll{quit: make(chan struct{}), done: make(chan struct{})}
	p.timer = time.AfterFunc(tcpCheckerSleepDuration, func() {
		defer close(p.done)
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("HeimDB: recovered from a panic in the closed-connection checker: %v\n%s", r, debug.Stack())
				cancel(&QueryPanicError{Value: r})
			}
		}()
		select {
		case <-p.quit: // stop の直後に始まったときは /proc を読まない
			return
		default:
		}
		if err := h.pollForClosedConnection(logger, tcpConn, p.quit); err != nil {
			cancel(err)
		}
	})
	return p
}

// stop は見張りを止める。始まっていれば、終わるまで待つ（文の終わりの後に context を取り消さない）。
func (p *closedConnectionPoll) stop() {
	close(p.quit)
	if !p.timer.Stop() {
		<-p.done
	}
}

// Periodically polls the connection socket to determine if it is has been closed by the client, returning an error
// if it has been. Returns immediately with no error on platforms that can't support TCP socket checks.
// HeimDB: it is started by startClosedConnectionPoll after tcpCheckerSleepDuration, checks at once and then every
// tcpCheckerSleepDuration, and returns when quit is closed (at the statement's end). It logs with the statement's
// logger, taken on the handler goroutine.
func (h *Handler) pollForClosedConnection(logger *logrus.Entry, tcpConn *net.TCPConn, quit <-chan struct{}) error {
	inode, err := getConnInode(tcpConn)
	if err != nil || inode == 0 {
		if !sockstate.ErrSocketCheckNotImplemented.Is(err) {
			logger.Trace("Connection checker exiting, connection isn't TCP")
		}
		return nil
	}

	t, ok := tcpConn.LocalAddr().(*net.TCPAddr)
	if !ok {
		logger.Trace("Connection checker exiting, could not get local port")
		return nil
	}

	timer := time.NewTimer(tcpCheckerSleepDuration)
	defer timer.Stop()

	for {
		st, err := getInodeSockState(t.Port, inode)
		switch st {
		case sockstate.Broken:
			logger.Warn("socket state is broken, returning error")
			return ErrConnectionWasClosed.New()
		case sockstate.Error:
			logger.WithError(err).Warn("Connection checker exiting, got err checking sockstate")
			return nil
		default: // Established
			// (juanjux) this check is not free, each iteration takes about 9 milliseconds to run on my machine
			// thus the small wait between checks
		}

		select {
		case <-quit:
			return nil
		case <-timer.C:
		}
		timer.Reset(tcpCheckerSleepDuration)
	}
}

func maybeGetTCPConn(conn net.Conn) (*net.TCPConn, bool) {
	wrap, ok := conn.(netutil.ConnWithTimeouts)
	if ok {
		conn = wrap.Conn
	}

	tcp, ok := conn.(*net.TCPConn)
	if ok {
		return tcp, true
	}

	return nil, false
}

func resultFromOkResult(result types.OkResult) *sqltypes.Result {
	infoStr := ""
	if result.Info != nil {
		infoStr = result.Info.String()
	}
	return &sqltypes.Result{
		RowsAffected: result.RowsAffected,
		InsertID:     result.InsertID,
		Info:         infoStr,
	}
}

// WarningCount is called at the end of each query to obtain
// the value to be returned to the client in the EOF packet.
// Note that this will be called either in the context of the
// ComQuery callback if the result does not contain any fields,
// or after the last ComQuery call completes.
func (h *Handler) WarningCount(c *mysql.Conn) uint16 {
	if sess := h.sm.session(c); sess != nil {
		return sess.WarningCount()
	}

	return 0
}

// getMaxUsedConnections returns the maximum number of connections that have been established at the same time for
// this sql-server, as tracked by the Max_used_connections status variable. If any error is encountered, it will be
// logged and 0 will be returned.
func getMaxUsedConnections() uint64 {
	_, maxUsedConnectionsValue, ok := sql.StatusVariables.GetGlobal("Max_used_connections")
	if !ok {
		logrus.Errorf("unable to find Max_used_connections status variable")
		return 0
	}
	maxUsedConnections, ok := maxUsedConnectionsValue.(uint64)
	if !ok {
		logrus.Errorf("unexpected type for Max_used_connections status variable: %T", maxUsedConnectionsValue)
		return 0
	}
	return maxUsedConnections
}

// getThreadsConnected returns the current number of connected threads, as tracked by the Threads_connected status
// variable. If any error is encountered, it will be logged and 0 will be returned.
func getThreadsConnected() uint64 {
	_, threadsConnectedValue, ok := sql.StatusVariables.GetGlobal("Threads_connected")
	if !ok {
		logrus.Errorf("unable to find Threads_connected status variable")
		return 0
	}
	threadsConnected, ok := threadsConnectedValue.(uint64)
	if !ok {
		logrus.Errorf("unexpected type for Threads_connected status variable: %T", threadsConnectedValue)
		return 0
	}
	return threadsConnected
}

// updateMaxUsedConnectionsStatusVariable updates the Max_used_connections status
// variables if the current number of connected threads is greater than the current
// value of Max_used_connections.
func updateMaxUsedConnectionsStatusVariable() {
	go func() {
		maxUsedConnections := getMaxUsedConnections()
		threadsConnected := getThreadsConnected()
		if threadsConnected > maxUsedConnections {
			sql.StatusVariables.SetGlobal("Max_used_connections", threadsConnected)
			// TODO: When Max_used_connections is updated, we should also update
			//       Max_used_connections_time with the current time, but our status
			//       variables support currently only supports Uint values.
		}
	}()
}

func toSqlHelper(ctx *sql.Context, typ sql.Type, buf *sql.ByteBuffer, val interface{}) (sqltypes.Value, error) {
	if buf == nil {
		return typ.SQL(ctx, nil, val)
	}
	ret, err := typ.SQL(ctx, buf.Get(), val)
	buf.Grow(ret.Len())
	return ret, err
}

func RowToSQL(ctx *sql.Context, sch sql.Schema, row sql.Row, projs []sql.Expression, buf *sql.ByteBuffer) ([]sqltypes.Value, error) {
	// need to make sure the schema is not null as some plan schema is defined as null (e.g. IfElseBlock)
	if len(sch) == 0 {
		return []sqltypes.Value{}, nil
	}

	outVals := make([]sqltypes.Value, len(sch))
	var err error
	if len(projs) == 0 {
		for i, col := range sch {
			if row[i] == nil {
				outVals[i] = sqltypes.NULL
				continue
			}
			outVals[i], err = toSqlHelper(ctx, col.Type, buf, row[i])
			if err != nil {
				return nil, err
			}
		}
		return outVals, nil
	}

	for i, col := range sch {
		field, err := projs[i].Eval(ctx, row)
		if err != nil {
			return nil, err
		}
		if field == nil {
			outVals[i] = sqltypes.NULL
			continue
		}
		outVals[i], err = toSqlHelper(ctx, col.Type, buf, field)
		if err != nil {
			return nil, err
		}
	}
	return outVals, nil
}

// notFixedDec は、小数の桁が決まっていない FLOAT と DOUBLE の列の情報の Decimals（MySQL の NOT_FIXED_DEC。HeimDB: 段階 118）。
const notFixedDec = 31

func schemaToFields(ctx *sql.Context, s sql.Schema) []*querypb.Field {
	charSetResults := ctx.GetCharacterSetResults()
	fields := make([]*querypb.Field, len(s))
	for i, c := range s {
		charset := uint32(sql.Collation_Default.CharacterSet())
		if collatedType, ok := c.Type.(sql.TypeWithCollation); ok {
			charset = uint32(collatedType.Collation().CharacterSet())
		}

		// Binary types always use a binary collation, but non-binary types must
		// respect character_set_results if it is set.
		if types.IsBinaryType(c.Type) {
			charset = uint32(sql.Collation_binary)
		} else if charSetResults != sql.CharacterSet_Unspecified {
			charset = uint32(charSetResults)
		}

		var flags querypb.MySqlFlag
		if !c.Nullable {
			flags = flags | querypb.MySqlFlag_NOT_NULL_FLAG
		}
		if c.AutoIncrement {
			flags = flags | querypb.MySqlFlag_AUTO_INCREMENT_FLAG
		}
		if c.PrimaryKey {
			flags = flags | querypb.MySqlFlag_PRI_KEY_FLAG
		}
		if types.IsUnsigned(c.Type) || types.RealUnsigned(c.Type) { // HeimDB: FLOAT UNSIGNED も（段階 120）
			flags = flags | querypb.MySqlFlag_UNSIGNED_FLAG
		}

		fields[i] = &querypb.Field{
			Name:         c.Name,
			OrgName:      c.Name,
			Table:        c.Source,
			OrgTable:     c.Source,
			Database:     c.DatabaseSource,
			Type:         c.Type.Type(),
			Charset:      charset,
			ColumnLength: c.Type.MaxTextResponseByteLength(ctx),
			Flags:        uint32(flags),
		}

		if types.IsDecimal(c.Type) {
			decimalType := c.Type.(sql.DecimalType)
			fields[i].Decimals = uint32(decimalType.Scale())
		} else if types.IsDatetimeType(c.Type) {
			dtType := c.Type.(sql.DatetimeType)
			fields[i].Decimals = uint32(dtType.Precision())
		} else if types.IsFloat(c.Type) {
			// HeimDB: FLOAT and DOUBLE columns and expressions are 31 (NOT_FIXED_DEC), as in
			// MySQL; with 0, mysqlnd read a FLOAT 1.5 as 2 over the binary protocol (stage 118).
			// FLOAT(M,D) and DOUBLE(M,D) and expressions of them are their decimals (stage 120)
			fields[i].Decimals = notFixedDec
			if scale, ok := types.RealScale(c.Type); ok {
				fields[i].Decimals = uint32(scale)
			}
		}
	}

	return fields
}

var (
	// QueryCounter describes a metric that accumulates number of queries monotonically.
	QueryCounter = discard.NewCounter()

	// QueryErrorCounter describes a metric that accumulates number of failed queries monotonically.
	QueryErrorCounter = discard.NewCounter()

	// QueryHistogram describes a queries latency.
	QueryHistogram = discard.NewHistogram()
)

func observeQuery(ctx *sql.Context, query string) func(err error) {
	span, ctx := ctx.Span("query", otel.WithAttributes(attribute.String("query", query)))

	t := time.Now()
	return func(err error) {
		if err != nil {
			QueryErrorCounter.With("query", query, "error", err.Error()).Add(1)
		} else {
			QueryCounter.With("query", query).Add(1)
			QueryHistogram.With("query", query, "duration", "seconds").Observe(time.Since(t).Seconds())
		}

		span.End()
	}
}

// QueryExecutor is a function that executes a query and returns the result as a schema and iterator. Either of
// |parsed| or |analyzed| can be nil depending on the use case
type QueryExecutor func(
	ctx *sql.Context,
	query string,
	parsed sqlparser.Statement,
	analyzed sql.Node,
	bindings map[string]*querypb.BindVariable,
	qFlags *sql.QueryFlags,
) (sql.Schema, sql.RowIter, *sql.QueryFlags, error)

// executeQuery is a QueryExecutor that calls QueryWithBindings on the given engine using the given query and parsed
// statement, which may be nil.
func (h *Handler) executeQuery(ctx *sql.Context, query string, parsed sqlparser.Statement, _ sql.Node, bindings map[string]*querypb.BindVariable, qFlags *sql.QueryFlags) (sql.Schema, sql.RowIter, *sql.QueryFlags, error) {
	bindingExprs, err := bindingsToExprs(bindings)
	if err != nil {
		return nil, nil, nil, err
	}
	return h.e.QueryWithBindings(ctx, query, parsed, bindingExprs, qFlags)
}

// executeQuery is a QueryExecutor that calls QueryWithBindings on the given engine using the given query and parsed
// statement, which may be nil.
func (h *Handler) executeBoundPlan(
	ctx *sql.Context,
	query string,
	_ sqlparser.Statement,
	plan sql.Node,
	_ map[string]*querypb.BindVariable,
	qFlags *sql.QueryFlags,
) (sql.Schema, sql.RowIter, *sql.QueryFlags, error) {
	return h.e.PrepQueryPlanForExecution(ctx, query, plan, qFlags)
}

func bindingsToExprs(bindings map[string]*querypb.BindVariable) (map[string]sqlparser.Expr, error) {
	res := make(map[string]sqlparser.Expr, len(bindings))
	for name, bv := range bindings {
		val, err := sqltypes.BindVariableToValue(bv)
		if err != nil {
			return nil, err
		}
		expr, err := sqlparser.ExprFromValue(val)
		if err != nil {
			return nil, err
		}
		res[name] = expr
	}
	return res, nil
}
