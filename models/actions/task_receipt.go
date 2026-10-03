// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package actions

import (
	"context"
	"errors"
	"fmt"

	"forgejo.org/models/db"
	"forgejo.org/modules/util"
)

// LockTaskRepository serializes assignment/enrollment with deletion in the same database transaction.
// A write also acquires SQLite's transaction lock before the receipt census is read.
func LockTaskRepository(ctx context.Context, repoID int64) error {
	if !db.InTransaction(ctx) {
		return errors.New("task repository lock requires a transaction")
	}
	_, err := db.Exec(ctx, "UPDATE `repository` SET id = id WHERE id = ?", repoID)
	if err != nil {
		return err
	}
	var row struct{ ID int64 }
	has, err := db.GetEngine(ctx).Table("repository").Where("id = ?", repoID).Get(&row)
	if err != nil {
		return err
	}
	if !has {
		return util.ErrNotExist
	}
	return nil
}

func GuardTaskDeletion(ctx context.Context, taskID int64) error {
	task, err := GetTaskByID(ctx, taskID)
	if errors.Is(err, util.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := LockTaskRepository(ctx, task.RepoID); err != nil {
		return err
	}
	task, err = GetTaskByID(ctx, taskID)
	if errors.Is(err, util.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if task.RunnerFinalReportReceived.Valid && !task.RunnerFinalReportReceived.Bool {
		return fmt.Errorf("task %d awaits runner final-report receipt", taskID)
	}
	return nil
}

func GuardRepositoryTaskDeletion(ctx context.Context, repoID int64) error {
	if err := LockTaskRepository(ctx, repoID); err != nil {
		return err
	}
	pending, err := db.GetEngine(ctx).Where("repo_id = ? AND runner_final_report_received = ?", repoID, false).Exist(new(ActionTask))
	if err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("repository %d has pending runner final-report receipts", repoID)
	}
	return nil
}

// EnrollTaskReceipt does not reset an already accepted receipt, including on late recovery.
func EnrollTaskReceipt(ctx context.Context, task *ActionTask) error {
	if err := LockTaskRepository(ctx, task.RepoID); err != nil {
		return err
	}
	current, err := GetTaskByID(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.RunnerID != task.RunnerID || current.RepoID != task.RepoID {
		return errors.New("recovered task assignment identity changed")
	}
	_, err = db.GetEngine(ctx).Exec("UPDATE `action_task` SET runner_final_report_received = ? WHERE id = ? AND runner_final_report_received IS NULL", false, task.ID)
	return err
}

func AcceptTaskFinalReport(ctx context.Context, taskID, runnerID int64) error {
	return db.WithTx(ctx, func(ctx context.Context) error {
		task, err := GetTaskByID(ctx, taskID)
		if err != nil {
			return err
		}
		if err := LockTaskRepository(ctx, task.RepoID); err != nil {
			return err
		}
		task, err = GetTaskByID(ctx, taskID)
		if err != nil {
			return err
		}
		if task.RunnerID != runnerID {
			return errors.New("invalid runner for final report")
		}
		_, err = db.GetEngine(ctx).Exec("UPDATE `action_task` SET runner_final_report_received = ? WHERE id = ? AND runner_id = ?", true, taskID, runnerID)
		return err
	})
}

// TaskReceiptCounts deliberately does not join action_runner: deleted identities cannot hide work.
func TaskReceiptCounts(ctx context.Context) (pending, uncovered, nonterminal int64, err error) {
	pending, err = db.GetEngine(ctx).Where("runner_final_report_received = ?", false).Count(new(ActionTask))
	if err != nil {
		return pending, uncovered, nonterminal, err
	}
	uncovered, err = db.GetEngine(ctx).Where("runner_id != 0 AND runner_final_report_received IS NULL").NotIn("status", DoneStatuses()).Count(new(ActionTask))
	if err != nil {
		return pending, uncovered, nonterminal, err
	}
	nonterminal, err = db.GetEngine(ctx).Where("runner_id != 0").NotIn("status", DoneStatuses()).Count(new(ActionTask))
	return pending, uncovered, nonterminal, err
}
