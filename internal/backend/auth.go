package backend

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/sirupsen/logrus"

	sqle "github.com/tsukinowasha/heimdb/internal/sqlengine"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql/mysql_db"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// 認証（docs/plan.md の段階 14）。
//
// go-mysql-server のユーザーと権限の仕組み（mysql_db）をそのまま使い、その直列化を m/users に保存する。ユーザーの
// 管理は MySQL と同じ CREATE USER、GRANT などで行う。

// EnableAuth は、エンジン e の認証を有効にする。保存したユーザーと権限を st から読み込み、以後の変更を st に保存する。
// まだユーザーがいなければ、パスワード rootPassword の root@'%'（全権限）を作る（created が true）。rootPassword が
// 空ならエラー。ユーザーがいれば rootPassword は使わない。
func EnableAuth(e *sqle.Engine, st *store.Store, rootPassword string) (created bool, err error) {
	db := e.Analyzer.Catalog.MySQLDb
	db.SetPersister(usersPersister{st})
	if st.IsReplica() {
		// 本番でユーザーと権限が変わったら、記録の適用の後に読み直す（docs/plan.md の段階 24）。
		st.OnApply(func(a store.Applied) {
			if !a.Users {
				return
			}
			if err := reloadUsers(db, st); err != nil {
				logrus.Errorf("replication: reload users: %v", err)
			}
		})
	}
	ctx := sql.NewEmptyContext()
	data, err := st.Users()
	if err != nil {
		return false, err
	}
	if len(data) > 0 {
		if err := db.LoadData(ctx, data); err != nil {
			return false, fmt.Errorf("load users: %w", err)
		}
		db.SetEnabled(true)
		return false, nil
	}
	if rootPassword == "" {
		return false, fmt.Errorf("no users yet: set the root password (-root-password or HEIMDB_ROOT_PASSWORD), or run with -no-auth")
	}
	ed := db.Editor()
	defer ed.Close()
	db.AddSuperUser(ed, "root", "%", rootPassword)
	return true, db.Persist(ctx, ed)
}

// siteUserPattern は、サイト用のユーザーの名前に使える文字。
var siteUserPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// EnsureSiteUser は、WordPress がつなぐサイト用のユーザー '<user>'@'%' を、データベース dbName だけの権限
// （GRANT ALL ON dbName.*）で作る（docs/plan.md の段階 32）。ユーザーがすでにいれば何もしない（パスワードと権限を
// 変えない）。WordPress を root でつながないため。作ったかどうかを返す。
//
// 権限は、普通の GRANT と同じになるよう、root として CREATE USER と GRANT を実行して与える。
func EnsureSiteUser(e *sqle.Engine, pro *Provider, user, password, dbName string) (bool, error) {
	if !siteUserPattern.MatchString(user) {
		return false, fmt.Errorf("invalid site user name %q (use letters, digits, '_' and '-')", user)
	}
	db := e.Analyzer.Catalog.MySQLDb
	rd := db.Reader()
	_, exists := rd.GetUser(mysql_db.UserPrimaryKey{User: user, Host: "%"})
	rd.Close()
	if exists {
		return false, nil
	}
	ctx := sql.NewContext(context.Background(), sql.WithSession(NewSession(
		sql.NewBaseSessionWithClientServer("", sql.Client{User: "root", Address: "localhost"}, 0), pro)))
	password = strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(password)
	for _, q := range []string{
		fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY '%s'", user, password),
		fmt.Sprintf("GRANT ALL ON `%s`.* TO '%s'@'%%'", strings.ReplaceAll(dbName, "`", "``"), user),
	} {
		_, iter, _, err := e.Query(ctx, q)
		if err == nil {
			_, err = sql.RowIterToRows(ctx, iter)
		}
		if err != nil {
			return false, fmt.Errorf("create site user %s (as root@'%%'; create it with CREATE USER and GRANT instead): %w", user, err)
		}
	}
	return true, nil
}

// reloadUsers は、保存したユーザーと権限で db の内容を置き換える。
func reloadUsers(db *mysql_db.MySQLDb, st *store.Store) error {
	data, err := st.Users()
	if err != nil || len(data) == 0 {
		return err
	}
	ed := db.Editor()
	defer ed.Close()
	if err := db.OverwriteUsersAndGrantData(sql.NewEmptyContext(), ed, data); err != nil {
		return err
	}
	db.SetEnabled(true)
	return nil
}

// usersPersister は、ユーザーと権限の変更を m/users に保存する mysql_db.MySQLDbPersistence。
type usersPersister struct {
	st *store.Store
}

var _ mysql_db.MySQLDbPersistence = usersPersister{}

func (p usersPersister) Persist(_ *sql.Context, data []byte) error {
	return p.st.SetUsers(data)
}

// noAuthPersister は、認証を有効にしていない（-no-auth）ときの MySQLDbPersistence。ユーザーの変更は保存せずエラーに
// する（保存先がないと、go-mysql-server は CREATE USER や GRANT で nil を参照して panic し、接続が切れる）。
// NewEngine が設定し、EnableAuth が usersPersister で置き換える。
type noAuthPersister struct{}

func (noAuthPersister) Persist(*sql.Context, []byte) error {
	return fmt.Errorf("user management is not available with -no-auth: restart with authentication enabled")
}

// ResetUsers は、起動時の -reset-passwords と -reset-users（docs/plan.md の段階 87）。退避から復元した store を、本番の
// パスワードを知らない環境（検証の環境など）で開けるようにする。
//
// root@'%' のパスワードを rootPassword にする（いなければ作る）。sitePassword があれば、siteUser@'%' がいるとき、
// そのパスワードを sitePassword にする（権限は変えない。いなければ main が EnsureSiteUser で作る）。dropOthers
// （-reset-users）なら、それ以外の DB のユーザー（本番のユーザー名とパスワードのハッシュ）をすべて消す。
// 消すのも変えるのも DB のユーザー（m/users）だけで、WordPress のユーザー（wp_users）と記事には触れない。
//
// 値がすでに同じなら書き込まない（旗を付けたままにしても、起動のたびに書き換えない）。書き込んだかと、
// -reset-passwords で残した、'%' でないホストの root とサイトのユーザー（本番のパスワードのまま。警告に出す）を返す。
// 本番で使うと、ALTER USER と同じくレプリカにも写る。
func ResetUsers(e *sqle.Engine, rootPassword, siteUser, sitePassword string, dropOthers bool) (changed bool, keptOtherHosts []string, err error) {
	if rootPassword == "" {
		return false, nil, fmt.Errorf("set the root password (-root-password or HEIMDB_ROOT_PASSWORD) to reset users")
	}
	db := e.Analyzer.Catalog.MySQLDb
	rootKey := mysql_db.UserPrimaryKey{User: "root", Host: "%"}
	siteKey := mysql_db.UserPrimaryKey{User: siteUser, Host: "%"}
	rootHash, siteHash := nativePasswordHash(rootPassword), nativePasswordHash(sitePassword)
	matches := func(u *mysql_db.User, hash string) bool {
		return u.AuthString == hash && !u.Locked && u.Plugin == "mysql_native_password"
	}

	rd := db.Reader()
	var others []mysql_db.UserPrimaryKey
	rootOK, siteFix := false, false
	rd.VisitUsers(func(u *mysql_db.User) {
		key := mysql_db.UserPrimaryKey{User: u.User, Host: u.Host}
		switch {
		case key == rootKey:
			rootOK = matches(u, rootHash)
		case sitePassword != "" && key == siteKey:
			siteFix = !matches(u, siteHash)
		case !u.IsEphemeral:
			others = append(others, key)
			if !dropOthers && (u.User == "root" || u.User == siteUser) {
				keptOtherHosts = append(keptOtherHosts, fmt.Sprintf("%s@'%s'", u.User, u.Host))
			}
		}
	})
	rd.Close()
	if rootOK && !siteFix && (!dropOthers || len(others) == 0) {
		return false, keptOtherHosts, nil
	}

	ed := db.Editor()
	defer ed.Close()
	if dropOthers {
		for _, k := range others {
			// DROP USER と同じく、ロールの結び付きも消す（同じ名前で作り直したユーザーが本番のロールを引き継がないように）
			ed.RemoveUser(k)
			ed.RemoveRoleEdgesFromKey(mysql_db.RoleEdgesFromKey{FromHost: k.Host, FromUser: k.User})
			ed.RemoveRoleEdgesToKey(mysql_db.RoleEdgesToKey{ToHost: k.Host, ToUser: k.User})
		}
	}
	if !rootOK {
		// AddSuperUser はいないときだけ作るので、消してから作る（すべての権限、ロックなし、mysql_native_password に戻る）。
		ed.RemoveUser(rootKey)
		db.AddSuperUser(ed, "root", "%", rootPassword)
	}
	if siteFix {
		if u, ok := ed.GetUser(siteKey); ok {
			c := *u // 読み手が持つ値を書き換えないよう、写してから変える
			c.AuthString, c.Plugin, c.Locked = siteHash, "mysql_native_password", false
			ed.RemoveUser(siteKey) // PutUser は値の全体で古いものを探すので、先に消す
			ed.PutUser(&c)
		}
	}
	return true, keptOtherHosts, db.Persist(sql.NewEmptyContext(), ed)
}

// nativePasswordHash は、mysql_native_password のパスワードのハッシュ（mysql_db の AddSuperUser と同じ。空なら空）。
func nativePasswordHash(password string) string {
	if password == "" {
		return ""
	}
	s1 := sha1.Sum([]byte(password))
	s2 := sha1.Sum(s1[:])
	return "*" + strings.ToUpper(hex.EncodeToString(s2[:]))
}
