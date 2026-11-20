package backend

import (
	"fmt"
	"strings"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/store"
)

// ブランチのユーザーが呼べる手続き（docs/plan.md の段階 63）。
//
// 起動フラグ -branch-procedures（Provider.BranchProcedures）を付けると、管理者でないユーザーも、接続中のデータベースの
// EXECUTE 権限で branchUserProcedures を呼べる。対象は接続中のデータベースのブランチだけで、main は対象にできない。
// ブランチ名に '' を渡すと、接続中のデータベースのブランチ（管理者も使える）。

// branchUserProcedures は、-branch-procedures でブランチのユーザーに開ける手続き。
var branchUserProcedures = map[string]bool{
	"heimdb_branch_diff":     true,
	"heimdb_branch_resolve":  true,
	"heimdb_changes":         true,
	"heimdb_restore_preview": true,
	"heimdb_restore":         true,
}

// adminOnly は、手続き name を管理者だけが呼べるかどうか。
func (p *Provider) adminOnly(name string) bool {
	name = strings.ToLower(name)
	return !publicProcedures[name] && !(p.BranchProcedures && branchUserProcedures[name])
}

// caller は、手続きを呼んだ接続。
type caller struct {
	// admin は、管理者（SUPER 権限）か、認証が無効なこと。
	admin bool
	// user は、記録に残すユーザー（'user'@'host'）。
	user string
}

// callerOf は、ctx の接続のユーザーを返す。
func (p *Provider) callerOf(ctx *sql.Context) caller {
	client := ctx.Session.Client()
	c := caller{admin: true, user: fmt.Sprintf("'%s'@'%s'", client.User, client.Address)}
	users := p.users
	if users == nil || !users.Enabled() {
		return c
	}
	rd := users.Reader()
	if u := users.GetUser(rd, client.User, client.Address, false); u != nil {
		c.user = fmt.Sprintf("'%s'@'%s'", u.User, u.Host) // CURRENT_USER() と同じく、アカウントの名前
	}
	rd.Close()
	c.admin = users.UserActivePrivilegeSet(ctx).Has(sql.PrivilegeType_Super)
	return c
}

// targetBranch は、手続き proc に渡されたブランチ名 name を、呼んだ接続に照らして確かめ、対象のブランチ名を返す。
// name が空なら、接続中のデータベースのブランチ。管理者でなければ、接続中のデータベースのブランチで、main でなく、
// そのデータベースの EXECUTE 権限を持つときだけ受け付ける（CALL をデータベース名で修飾しても、別のブランチは指せない）。
func (p *Provider) targetBranch(ctx *sql.Context, proc, name string) (string, caller, error) {
	c := p.callerOf(ctx)
	cur := ctx.GetCurrentDatabase()
	db, connected := p.database(cur)
	if name == "" {
		if !connected {
			return "", c, fmt.Errorf("HeimDB: %s('') needs a connection to a branch's database (connected to %q)", proc, cur)
		}
		name = "main"
		if _, n, ok := strings.Cut(db.name, "@"); ok {
			name = n
		}
	}
	if c.admin {
		return name, c, nil
	}
	b, ok := p.st.BranchByName(name)
	switch {
	case !connected || !ok || b.ID != db.branch:
		return "", c, fmt.Errorf("HeimDB: a branch user can call %s only on the branch it is connected to (connected to %q)", proc, cur)
	case b.ID == store.MainBranchID:
		return "", c, fmt.Errorf("HeimDB: only an administrator can call %s on the main branch", proc)
	}
	op := sql.NewPrivilegedOperation(sql.PrivilegeCheckSubject{Database: cur}, sql.PrivilegeType_Execute)
	if !p.users.UserHasPrivileges(ctx, op) {
		return "", c, fmt.Errorf("HeimDB: %s needs the EXECUTE privilege on %s", proc, cur)
	}
	return b.Name, c, nil
}

// checkBranchTime は、ブランチのユーザーが、ブランチ name を作る前の時刻 at を渡していればエラーにする（その時刻の
// ブランチは親の状態で、分岐の後に親で変わる前の親の履歴が見える。段階 63）。管理者は何も確かめない。
func (p *Provider) checkBranchTime(ctx *sql.Context, c caller, name, at string) error {
	if c.admin {
		return nil
	}
	ts, err := parseAsOf(ctx, at)
	if err != nil {
		return err
	}
	if b, ok := p.st.BranchByName(name); ok && ts < b.ForkedAt {
		return fmt.Errorf("HeimDB: a branch user cannot use a time before the branch was created (%s)", formatRestoreTime(b.ForkedAt))
	}
	return nil
}

// onBranch は、手続き proc を、targetBranch で確かめた対象のブランチ名で呼ぶ。
func (p *Provider) onBranch(ctx *sql.Context, proc, name string, fn func(name string, c caller) (sql.RowIter, error)) (sql.RowIter, error) {
	name, c, err := p.targetBranch(ctx, proc, name)
	if err != nil {
		return nil, err
	}
	return fn(name, c)
}
