package backend

import (
	"fmt"
	"time"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"
)

// ビュー、トリガー、ストアドプロシージャ、イベント（WordPress は使わない）。
//
// HeimDB はこれらを保存しない。認証を有効にすると、go-mysql-server はデータベースを mysql_db.PrivilegedDatabase で
// 包み、これらのインターフェースをすべて HeimDB に転送する（実装がなければエラーになり、CREATE TABLE が同じ名前の
// ビューを確かめるところで失敗する）。読む側は「ない」を返し、作る側は明示的なエラーにする（docs/plan.md の段階 14）。

var (
	_ sql.ViewDatabase            = (*Database)(nil)
	_ sql.TriggerDatabase         = (*Database)(nil)
	_ sql.StoredProcedureDatabase = (*Database)(nil)
	_ sql.EventDatabase           = (*Database)(nil)
)

func errUnsupportedObject(kind string) error {
	return fmt.Errorf("HeimDB: %s are not supported", kind)
}

// CreateView は sql.ViewDatabase の実装。
func (d *Database) CreateView(*sql.Context, string, string, string) error {
	return errUnsupportedObject("views")
}

// DropView は sql.ViewDatabase の実装。
func (d *Database) DropView(_ *sql.Context, name string) error {
	return sql.ErrViewDoesNotExist.New(d.name, name)
}

// GetViewDefinition は sql.ViewDatabase の実装。
func (d *Database) GetViewDefinition(*sql.Context, string) (sql.ViewDefinition, bool, error) {
	return sql.ViewDefinition{}, false, nil
}

// AllViews は sql.ViewDatabase の実装。
func (d *Database) AllViews(*sql.Context) ([]sql.ViewDefinition, error) { return nil, nil }

// GetTriggers は sql.TriggerDatabase の実装。
func (d *Database) GetTriggers(*sql.Context) ([]sql.TriggerDefinition, error) { return nil, nil }

// CreateTrigger は sql.TriggerDatabase の実装。
func (d *Database) CreateTrigger(*sql.Context, sql.TriggerDefinition) error {
	return errUnsupportedObject("triggers")
}

// DropTrigger は sql.TriggerDatabase の実装。
func (d *Database) DropTrigger(_ *sql.Context, name string) error {
	return sql.ErrTriggerDoesNotExist.New(name)
}

// GetStoredProcedure は sql.StoredProcedureDatabase の実装。
func (d *Database) GetStoredProcedure(*sql.Context, string) (sql.StoredProcedureDetails, bool, error) {
	return sql.StoredProcedureDetails{}, false, nil
}

// GetStoredProcedures は sql.StoredProcedureDatabase の実装。
func (d *Database) GetStoredProcedures(*sql.Context) ([]sql.StoredProcedureDetails, error) {
	return nil, nil
}

// SaveStoredProcedure は sql.StoredProcedureDatabase の実装。
func (d *Database) SaveStoredProcedure(*sql.Context, sql.StoredProcedureDetails) error {
	return errUnsupportedObject("stored procedures")
}

// DropStoredProcedure は sql.StoredProcedureDatabase の実装。
func (d *Database) DropStoredProcedure(_ *sql.Context, name string) error {
	return sql.ErrStoredProcedureDoesNotExist.New(name)
}

// GetEvent は sql.EventDatabase の実装。
func (d *Database) GetEvent(*sql.Context, string) (sql.EventDefinition, bool, error) {
	return sql.EventDefinition{}, false, nil
}

// GetEvents は sql.EventDatabase の実装。
func (d *Database) GetEvents(*sql.Context) ([]sql.EventDefinition, interface{}, error) {
	return nil, nil, nil
}

// SaveEvent は sql.EventDatabase の実装。
func (d *Database) SaveEvent(*sql.Context, sql.EventDefinition) (bool, error) {
	return false, errUnsupportedObject("events")
}

// DropEvent は sql.EventDatabase の実装。
func (d *Database) DropEvent(_ *sql.Context, name string) error {
	return sql.ErrEventDoesNotExist.New(name)
}

// UpdateEvent は sql.EventDatabase の実装。
func (d *Database) UpdateEvent(*sql.Context, string, sql.EventDefinition) (bool, error) {
	return false, errUnsupportedObject("events")
}

// UpdateLastExecuted は sql.EventDatabase の実装。
func (d *Database) UpdateLastExecuted(*sql.Context, string, time.Time) error {
	return errUnsupportedObject("events")
}

// NeedsToReloadEvents は sql.EventDatabase の実装。
func (d *Database) NeedsToReloadEvents(*sql.Context, interface{}) (bool, error) { return false, nil }
