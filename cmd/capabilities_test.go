// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"forgejo.org/modules/structs"

	"github.com/stretchr/testify/require"
)

func TestCapabilitiesStandalone(t *testing.T) {
	directory := t.TempDir()
	t.Chdir(directory)
	t.Setenv("GITEA_WORK_DIR", filepath.Join(directory, "absent-work"))
	t.Setenv("GITEA__database__DB_TYPE", "invalid-database")
	// An existing directory cannot be read as an app.ini file. Configuration-dependent commands fail here.
	result, err := runTestApp(NewMainApp("test", ""), "forgejo", "--config", directory, "capabilities")
	require.NoError(t, err)
	require.Empty(t, result.Stderr)
	var document struct {
		Schema       int      `json:"schema"`
		Capabilities []string `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Stdout), &document))
	require.Equal(t, 1, document.Schema)
	require.Equal(t, []string{structs.ServerCapabilityActionsAdmissionDrain}, document.Capabilities)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries)
	for _, argument := range []string{"unexpected", "--unexpected"} {
		result, err = runTestApp(NewMainApp("test", ""), "forgejo", "capabilities", argument)
		require.Error(t, err)
		require.Equal(t, 1, result.ExitCode)
	}
}

type capabilitiesFailWriter struct{}

func (capabilitiesFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCapabilitiesEncodingFailure(t *testing.T) {
	app := NewMainApp("test", "")
	app.Writer = capabilitiesFailWriter{}
	require.ErrorIs(t, app.Run(context.Background(), []string{"forgejo", "capabilities"}), io.ErrClosedPipe)
}
