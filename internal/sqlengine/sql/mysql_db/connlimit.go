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

// HeimDB が足したファイル（docs/engine.md）。アカウントごとの同時接続数の上限（MAX_USER_CONNECTIONS）を効かせる。

package mysql_db

import (
	"net"
	"sync"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// connLimiter は、上限のあるアカウントの接続を数える。認証が通った直後に数え（超えていればエラー 1203 で拒否する）、
// 接続が切れたとき（ConnectionClosed）に戻す。
type connLimiter struct {
	mu     sync.Mutex
	conns  map[uint32]UserPrimaryKey // 接続 ID ごとの、数えたアカウント
	byUser map[UserPrimaryKey]uint32 // アカウントごとの接続の数
}

// acquire は、接続 connID をアカウント u の接続として数える。上限 limit（0 なら上限なし）を超えるならエラー 1203。
func (l *connLimiter) acquire(connID uint32, u UserPrimaryKey, limit uint32) error {
	if limit == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conns == nil {
		l.conns, l.byUser = map[uint32]UserPrimaryKey{}, map[UserPrimaryKey]uint32{}
	}
	if _, ok := l.conns[connID]; ok {
		return nil
	}
	if l.byUser[u] >= limit {
		return mysql.NewSQLError(mysql.ERTooManyUserConnections, "42000", "User %s already has more than 'max_user_connections' active connections", u.User)
	}
	l.conns[connID] = u
	l.byUser[u]++
	return nil
}

// release は、接続 connID を数えていれば戻す。
func (l *connLimiter) release(connID uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, ok := l.conns[connID]
	if !ok {
		return
	}
	delete(l.conns, connID)
	if l.byUser[u]--; l.byUser[u] == 0 {
		delete(l.byUser, u)
	}
}

// ConnectionClosed は、接続が切れたときに、その接続を数えていれば戻す（server.Handler が呼ぶ）。
func (db *MySQLDb) ConnectionClosed(connID uint32) {
	db.connLimiter.release(connID)
}

// limitedAuthMethod は、認証が通った後に、アカウントの同時接続数の上限を確かめる mysql.AuthMethod。
type limitedAuthMethod struct {
	mysql.AuthMethod
	db *MySQLDb
}

func (m limitedAuthMethod) HandleAuthPluginData(c *mysql.Conn, user string, serverAuthPluginData []byte, clientAuthPluginData []byte, remoteAddr net.Addr) (mysql.Getter, error) {
	getter, err := m.AuthMethod.HandleAuthPluginData(c, user, serverAuthPluginData, clientAuthPluginData, remoteAddr)
	if err != nil || !m.db.Enabled() {
		return getter, err
	}
	cu, ok := getter.(sql.MysqlConnectionUser)
	if !ok {
		return getter, nil
	}
	// 認証の方式によって、Host はアカウントのホスト（'%' など）か接続元のアドレスなので、パターンで照合して引く。
	rd := m.db.Reader()
	entry := m.db.GetUser(rd, cu.User, cu.Host, false)
	rd.Close()
	if entry == nil {
		return getter, nil
	}
	pk := UserPrimaryKey{User: entry.User, Host: entry.Host}
	if err := m.db.connLimiter.acquire(c.ConnectionID, pk, entry.MaxUserConnections); err != nil {
		return nil, err
	}
	return getter, nil
}
