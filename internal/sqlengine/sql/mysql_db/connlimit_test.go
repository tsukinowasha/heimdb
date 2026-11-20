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

// HeimDB が足したファイル（docs/engine.md）。

package mysql_db

import (
	"net"
	"testing"

	"github.com/dolthub/vitess/go/mysql"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// hostAuthMethod は、認証を通し、ホストに host を入れた接続のユーザーを返す mysql.AuthMethod（テスト用）。
type hostAuthMethod struct {
	mysql.AuthMethod
	host string
}

func (m hostAuthMethod) HandleAuthPluginData(_ *mysql.Conn, user string, _, _ []byte, _ net.Addr) (mysql.Getter, error) {
	return sql.MysqlConnectionUser{User: user, Host: m.host}, nil
}

// 認証の方式がアカウントのホスト（'%'）を返しても、接続元のアドレスを返しても（パスワードのない
// caching_sha2_password）、MAX_USER_CONNECTIONS で数える。
func TestLimitedAuthMethodHost(t *testing.T) {
	db := CreateEmptyMySQLDb()
	db.SetEnabled(true)
	ed := db.Editor()
	ed.PutUser(&User{User: "stg", Host: "%", PrivilegeSet: NewPrivilegeSet(), MaxUserConnections: 1})
	ed.Close()

	for i, host := range []string{"%", "192.0.2.1"} {
		m := limitedAuthMethod{hostAuthMethod{host: host}, db}
		first, second := uint32(10*i+1), uint32(10*i+2)
		if _, err := m.HandleAuthPluginData(&mysql.Conn{ConnectionID: first}, "stg", nil, nil, nil); err != nil {
			t.Fatalf("host %s: first: %v", host, err)
		}
		if _, err := m.HandleAuthPluginData(&mysql.Conn{ConnectionID: second}, "stg", nil, nil, nil); err == nil {
			t.Errorf("host %s: second connection should fail", host)
		}
		db.ConnectionClosed(first)
		if _, err := m.HandleAuthPluginData(&mysql.Conn{ConnectionID: second}, "stg", nil, nil, nil); err != nil {
			t.Errorf("host %s: after close: %v", host, err)
		}
		db.ConnectionClosed(second)
	}
}

// 保存先がなくても、Persist は panic せずにエラーを返す（もとは nil を参照して panic した）。
func TestPersistWithoutPersister(t *testing.T) {
	db := CreateEmptyMySQLDb()
	ed := db.Editor()
	defer ed.Close()
	ed.PutUser(&User{User: "u", Host: "%", PrivilegeSet: NewPrivilegeSet()})
	if err := db.Persist(sql.NewEmptyContext(), ed); err == nil {
		t.Errorf("Persist without a persister succeeded")
	}
}
