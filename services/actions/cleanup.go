// Copyright 2023 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	actions_model "forgejo.org/models/actions"
	"forgejo.org/models/db"
	actions_module "forgejo.org/modules/actions"
	"forgejo.org/modules/log"
	"forgejo.org/modules/setting"
	"forgejo.org/modules/storage"
	"forgejo.org/modules/timeutil"
	"forgejo.org/modules/util"

	"xorm.io/builder"
)

// Cleanup removes expired actions logs, data, artifacts and used ephemeral runners
func Cleanup(ctx context.Context) error {
	// clean up expired artifacts
	if err := CleanupArtifacts(ctx); err != nil {
		return fmt.Errorf("failed to clean up artifacts: %w", err)
	}

	// clean up old logs
	if err := CleanupLogs(ctx); err != nil {
		return fmt.Errorf("failed to clean up logs: %w", err)
	}

	// clean up old ephemeral runners
	if err := CleanupEphemeralRunners(ctx); err != nil {
		return fmt.Errorf("failed to clean up old ephemeral runners: %w", err)
	}

	return nil
}

// CleanupArtifacts removes expired add need-deleted artifacts and set records expired status
func CleanupArtifacts(taskCtx context.Context) error {
	if err := cleanExpiredArtifacts(taskCtx); err != nil {
		return err
	}
	return cleanNeedDeleteArtifacts(taskCtx)
}

func cleanExpiredArtifacts(taskCtx context.Context) error {
	artifacts, err := actions_model.ListNeedExpiredArtifacts(taskCtx)
	if err != nil {
		return err
	}
	log.Info("Found %d expired artifacts", len(artifacts))
	for _, artifact := range artifacts {
		if err := actions_model.SetArtifactExpired(taskCtx, artifact.ID); err != nil {
			log.Error("Cannot set artifact %d expired: %v", artifact.ID, err)
			continue
		}
		if err := storage.ActionsArtifacts.Delete(artifact.StoragePath); err != nil {
			log.Error("Cannot delete artifact %d: %v", artifact.ID, err)
			continue
		}
		log.Info("Artifact %d set expired", artifact.ID)
	}
	return nil
}

// deleteArtifactBatchSize is the batch size of deleting artifacts
const deleteArtifactBatchSize = 100

func cleanNeedDeleteArtifacts(taskCtx context.Context) error {
	for {
		artifacts, err := actions_model.ListPendingDeleteArtifacts(taskCtx, deleteArtifactBatchSize)
		if err != nil {
			return err
		}
		log.Info("Found %d artifacts pending deletion", len(artifacts))
		for _, artifact := range artifacts {
			if err := actions_model.SetArtifactDeleted(taskCtx, artifact.ID); err != nil {
				log.Error("Cannot set artifact %d deleted: %v", artifact.ID, err)
				continue
			}
			if err := storage.ActionsArtifacts.Delete(artifact.StoragePath); err != nil {
				log.Error("Cannot delete artifact %d: %v", artifact.ID, err)
				continue
			}
			log.Info("Artifact %d set deleted", artifact.ID)
		}
		if len(artifacts) < deleteArtifactBatchSize {
			log.Debug("No more artifacts pending deletion")
			break
		}
	}
	return nil
}

const deleteLogBatchSize = 100

// CleanupLogs removes logs which are older than the configured retention time
func CleanupLogs(ctx context.Context) error {
	return cleanupLogs(ctx, actions_model.FindOldTasksToExpire)
}

func cleanupLogs(ctx context.Context, findTasks func(context.Context, timeutil.TimeStamp, int, int64) ([]*actions_model.ActionTask, error)) error {
	olderThan := timeutil.TimeStampNow().AddDuration(-time.Duration(setting.Actions.LogRetentionDays) * 24 * time.Hour)

	count := 0
	var afterID int64
	for {
		tasks, err := findTasks(ctx, olderThan, deleteLogBatchSize, afterID)
		if err != nil {
			return fmt.Errorf("could not retrieve tasks to expire: %w", err)
		}
		for _, task := range tasks {
			afterID = task.ID
			expired, err := expireTaskLogs(ctx, task.ID, olderThan)
			if err != nil {
				log.Error("Failed to expire logs of task %v: %v", task.ID, err)
				continue
			}
			if expired {
				count++
			}
		}
		if len(tasks) < deleteLogBatchSize {
			break
		}
	}

	log.Info("Removed %d logs", count)
	return nil
}

func expireTaskLogs(ctx context.Context, taskID int64, olderThan timeutil.TimeStamp) (expired bool, resultErr error) {
	resultErr = db.WithTx(ctx, func(ctx context.Context) error {
		task, err := actions_model.GetTaskByID(ctx, taskID)
		if errors.Is(err, util.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := actions_model.LockTaskRepository(ctx, task.RepoID); err != nil {
			return err
		}
		task, err = actions_model.GetTaskByID(ctx, taskID)
		if errors.Is(err, util.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if task.LogExpired || task.Stopped <= 0 || task.Stopped >= olderThan || (task.RunnerFinalReportReceived.Valid && !task.RunnerFinalReportReceived.Bool) {
			return nil
		}
		if task.HasLogs() {
			if err := actions_module.RemoveLogs(ctx, task.LogInStorage, task.LogFilename); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		task.LogIndexes = nil
		task.LogExpired = true
		if err := actions_model.UpdateTask(ctx, task, "log_indexes", "log_expired"); err != nil {
			return err
		}
		expired = true
		return nil
	})
	return expired, resultErr
}

// CleanupEphemeralRunners removes used ephemeral runners which are no longer able to process jobs
func CleanupEphemeralRunners(ctx context.Context) error {
	var ids []int
	err := db.GetEngine(ctx).
		Table("`action_runner`").
		Select("DISTINCT `action_runner`.id").
		Join("INNER", "`action_task`", "`action_task`.`runner_id` = `action_runner`.`id`").
		Where(builder.Eq{"`action_runner`.`ephemeral`": true}).
		And(builder.In("`action_task`.`status`", actions_model.DoneStatuses())).
		Find(&ids)
	if err != nil {
		return fmt.Errorf("failed to find ephemeral runners: %w", err)
	}

	res, err := db.GetEngine(ctx).
		In("id", ids).
		Delete(&actions_model.ActionRunner{})
	if err != nil {
		return fmt.Errorf("failed to delete ephemeral runners: %w", err)
	}

	log.Info("Removed %d ephemeral runners", res)
	return nil
}

// CleanupEphemeralRunnersByPickedTaskOfRepo removes all ephemeral runners that have active/finished tasks on the given repository
func CleanupEphemeralRunnersByPickedTaskOfRepo(ctx context.Context, repoID int64) error {
	subQuery := builder.Select("`action_runner`.id").
		From(builder.Select("*").From("`action_runner`"), "`action_runner`"). // mysql needs this redundant subquery
		Join("INNER", "`action_task`", "`action_task`.`runner_id` = `action_runner`.`id`").
		Where(builder.And(builder.Eq{"`action_runner`.`ephemeral`": true}, builder.Eq{"`action_task`.`repo_id`": repoID}))
	b := builder.Delete(builder.In("id", subQuery)).From("`action_runner`")
	res, err := db.GetEngine(ctx).Exec(b)
	if err != nil {
		return fmt.Errorf("find runners: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	log.Info("Removed %d runners", affected)
	return nil
}

// CleanupOfflineRunners removes offline runners
func CleanupOfflineRunners(ctx context.Context, duration time.Duration, globalOnly bool) error {
	olderThan := timeutil.TimeStampNow().AddDuration(-duration)
	return actions_model.DeleteOfflineRunners(ctx, olderThan, globalOnly)
}
