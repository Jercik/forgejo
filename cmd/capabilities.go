// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package cmd

import (
	"context"
	"encoding/json"

	"forgejo.org/modules/structs"

	"github.com/urfave/cli/v3"
)

// cmdCapabilities reports compiled protocol support without loading configuration.
func cmdCapabilities() *cli.Command {
	return &cli.Command{
		Name:   "capabilities",
		Usage:  "Print compiled maintenance capabilities as JSON",
		Before: noDanglingArgs,
		Action: func(_ context.Context, command *cli.Command) error {
			return json.NewEncoder(command.Root().Writer).Encode(struct {
				Schema       int      `json:"schema"`
				Capabilities []string `json:"capabilities"`
			}{1, []string{structs.ServerCapabilityActionsAdmissionDrain}})
		},
	}
}
