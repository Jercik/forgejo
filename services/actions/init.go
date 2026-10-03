// Copyright 2022 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package actions

import (
	actions_module "forgejo.org/modules/actions"
	"forgejo.org/modules/graceful"
	"forgejo.org/modules/log"
	"forgejo.org/modules/queue"
	"forgejo.org/modules/setting"
	notify_service "forgejo.org/services/notify"
)

func Init() {
	if err := actions_module.InitAdmissionLock(setting.Actions.AdmissionLockPath); err != nil {
		log.Fatal("Unable to initialize actions admission lock: %v", err)
	}

	if !setting.Actions.Enabled {
		return
	}

	jobEmitterQueue = queue.CreateUniqueQueue(graceful.GetManager().ShutdownContext(), "actions_ready_job", jobEmitterQueueHandler)
	if jobEmitterQueue == nil {
		log.Fatal("Unable to create actions_ready_job queue")
	}
	go graceful.GetManager().RunWithCancel(jobEmitterQueue)

	notify_service.RegisterNotifier(NewNotifier())
}
