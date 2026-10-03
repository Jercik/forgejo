// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package forgejo_migrations

import (
	"database/sql"

	"code.forgejo.org/xorm/xorm"
)

func init() {
	registerMigration(&Migration{Description: "j4k: track runner final-report acceptance", Upgrade: addTaskFinalReceipt})
}

func addTaskFinalReceipt(x *xorm.Engine) error {
	type ActionTask struct {
		RunnerFinalReportReceived sql.NullBool `xorm:"DEFAULT NULL"`
	}
	_, err := x.SyncWithOptions(xorm.SyncOptions{IgnoreDropIndices: true}, new(ActionTask))
	return err
}
