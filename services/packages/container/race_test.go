// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package container

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"forgejo.org/models/db"
	packages_model "forgejo.org/models/packages"
	container_model "forgejo.org/models/packages/container"
	"forgejo.org/models/unittest"
	user_model "forgejo.org/models/user"
	packages_module "forgejo.org/modules/packages"
	packages_service "forgejo.org/services/packages"

	"code.forgejo.org/xorm/xorm"
	"code.forgejo.org/xorm/xorm/contexts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The race tests simulate the loser of a concurrent push. SQLite serializes
// transactions, so a real second request would simply see the winner's rows.
// Instead, a SQL hook makes the loser's first lookup of the winner's committed
// row miss (it changes the looked-up value), so the following INSERT hits the
// real unique constraint, as the loser does on PostgreSQL, where the winner's
// commit lands between the loser's lookup and insert. The retry after the
// violation and its outcome are real. What these tests cannot reproduce is
// PostgreSQL aborting the transaction after the failed statement; that is why
// the production code retries the whole transaction instead of continuing in it.

type sqlHook struct {
	mu   sync.Mutex
	rule func(c *contexts.ContextHook) error
	// uniqueViolations counts statements the database rejected with a unique constraint error.
	uniqueViolations int
}

func (h *sqlHook) BeforeProcess(c *contexts.ContextHook) (context.Context, error) {
	h.mu.Lock()
	rule := h.rule
	h.mu.Unlock()
	if rule != nil {
		if err := rule(c); err != nil {
			return nil, err
		}
	}
	return c.Ctx, nil
}

func (h *sqlHook) AfterProcess(c *contexts.ContextHook) error {
	if c.Err != nil && strings.Contains(c.Err.Error(), "UNIQUE constraint failed") {
		h.mu.Lock()
		h.uniqueViolations++
		h.mu.Unlock()
	}
	return nil
}

var (
	raceHook     = &sqlHook{}
	raceHookOnce sync.Once
)

func installRaceHook(t *testing.T, rule func(c *contexts.ContextHook) error) *sqlHook {
	t.Helper()
	raceHookOnce.Do(func() {
		engine, ok := db.GetEngine(db.DefaultContext).(*xorm.Engine)
		require.True(t, ok)
		engine.AddHook(raceHook)
	})
	raceHook.mu.Lock()
	raceHook.rule = rule
	raceHook.uniqueViolations = 0
	raceHook.mu.Unlock()
	t.Cleanup(func() {
		raceHook.mu.Lock()
		raceHook.rule = nil
		raceHook.mu.Unlock()
	})
	return raceHook
}

func (h *sqlHook) violations() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.uniqueViolations
}

// missFirstLookup makes the first statement containing sqlPart look up a value
// that does not exist, by rewriting the first argument of the given type.
func missFirstLookup(sqlPart string, rewrite func(arg any) (any, bool)) func(c *contexts.ContextHook) error {
	done := false
	return func(c *contexts.ContextHook) error {
		if done || !strings.HasPrefix(c.SQL, "SELECT") || !strings.Contains(c.SQL, sqlPart) {
			return nil
		}
		for i, arg := range c.Args {
			if replaced, ok := rewrite(arg); ok {
				c.Args[i] = replaced
				done = true
				return nil
			}
		}
		return nil
	}
}

func rewriteSize(arg any) (any, bool) {
	size, ok := arg.(int64)
	return size + 1, ok
}

func rewriteVersion(arg any) (any, bool) {
	version, ok := arg.(string)
	return version + "-not-visible-yet", ok
}

var errInjected = errors.New("injected failure")

// failInsert fails every INSERT into table whose arguments include marker.
func failInsert(table, marker string) func(c *contexts.ContextHook) error {
	return func(c *contexts.ContextHook) error {
		if !strings.HasPrefix(c.SQL, "INSERT INTO `"+table+"`") {
			return nil
		}
		for _, arg := range c.Args {
			if s, ok := arg.(string); ok && s == marker {
				return errInjected
			}
		}
		return nil
	}
}

// failInsertWithViolation fails every INSERT into table whose arguments include marker as the database would
// report a unique violation, and counts the INSERTs into countedTable.
func failInsertWithViolation(table, marker, countedTable string, count *int) func(c *contexts.ContextHook) error {
	return func(c *contexts.ContextHook) error {
		if strings.HasPrefix(c.SQL, "INSERT INTO `"+countedTable+"`") {
			*count++
		}
		if err := failInsert(table, marker)(c); err != nil {
			return fmt.Errorf("%w: %w", xorm.ErrUniqueConstraintViolation, errInjected)
		}
		return nil
	}
}

func chainRules(rules ...func(c *contexts.ContextHook) error) func(c *contexts.ContextHook) error {
	return func(c *contexts.ContextHook) error {
		for _, rule := range rules {
			if err := rule(c); err != nil {
				return err
			}
		}
		return nil
	}
}

func hashedBuffer(t *testing.T, content string) *packages_module.HashedBuffer {
	t.Helper()
	buf, err := packages_module.CreateHashedBufferFromReaderWithSize(strings.NewReader(content), MaxManifestSize+1)
	require.NoError(t, err)
	t.Cleanup(func() { buf.Close() })
	return buf
}

func blobCreationInfo(owner *user_model.User, image string) *packages_service.PackageCreationInfo {
	return &packages_service.PackageCreationInfo{
		PackageInfo: packages_service.PackageInfo{Owner: owner, Name: image},
		Creator:     owner,
	}
}

func contentExists(buf *packages_module.HashedBuffer) bool {
	_, _, sha256, _, _ := buf.Sums()
	return packages_module.NewContentStore().Has(packages_module.BlobHash256Key(hex.EncodeToString(sha256))) == nil
}

func countBlobs(t *testing.T, buf *packages_module.HashedBuffer) int64 {
	t.Helper()
	pb := packages_service.NewPackageBlob(buf)
	n, err := db.GetEngine(t.Context()).Where("hash_sha256 = ?", pb.HashSHA256).Count(&packages_model.PackageBlob{})
	require.NoError(t, err)
	return n
}

func TestSaveAsPackageBlobLostInsertRace(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	winnerBuf := hashedBuffer(t, "race test blob: lost insert")
	_, err := SaveAsPackageBlob(t.Context(), winnerBuf, blobCreationInfo(user2, "race-blob-winner"))
	require.NoError(t, err)
	require.True(t, contentExists(winnerBuf))

	hook := installRaceHook(t, missFirstLookup("FROM `package_blob` WHERE hash_md5=?", rewriteSize))

	loserBuf := hashedBuffer(t, "race test blob: lost insert")
	pb, err := SaveAsPackageBlob(t.Context(), loserBuf, blobCreationInfo(user2, "race-blob-loser"))
	require.NoError(t, err)

	assert.Equal(t, 1, hook.violations(), "the loser must have lost the insert")
	assert.EqualValues(t, 1, countBlobs(t, winnerBuf))
	assert.Equal(t, packages_service.NewPackageBlob(winnerBuf).HashSHA256, pb.HashSHA256)
	assert.True(t, contentExists(winnerBuf))
}

func TestSaveAsPackageBlobLostInsertRaceThenFailureKeepsWinnersContent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	winnerBuf := hashedBuffer(t, "race test blob: lost insert then failure")
	_, err := SaveAsPackageBlob(t.Context(), winnerBuf, blobCreationInfo(user2, "race-blob-failure-winner"))
	require.NoError(t, err)

	hook := installRaceHook(t, chainRules(
		missFirstLookup("FROM `package_blob` WHERE hash_md5=?", rewriteSize),
		failInsert("package_file", "sha256_"+packages_service.NewPackageBlob(winnerBuf).HashSHA256),
	))

	loserBuf := hashedBuffer(t, "race test blob: lost insert then failure")
	_, err = SaveAsPackageBlob(t.Context(), loserBuf, blobCreationInfo(user2, "race-blob-failure-loser"))
	require.ErrorIs(t, err, errInjected)

	assert.Equal(t, 1, hook.violations(), "the loser must have lost the insert")
	assert.EqualValues(t, 1, countBlobs(t, winnerBuf))
	assert.True(t, contentExists(winnerBuf), "the loser's cleanup deleted the winner's content")
}

func TestSaveAsPackageBlobFailureRemovesOwnContent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	buf := hashedBuffer(t, "race test blob: failure without winner")
	installRaceHook(t, failInsert("package_file", "sha256_"+packages_service.NewPackageBlob(buf).HashSHA256))

	_, err := SaveAsPackageBlob(t.Context(), buf, blobCreationInfo(user2, "race-blob-own-content"))
	require.ErrorContains(t, err, errInjected.Error())

	assert.False(t, contentExists(buf))
	assert.EqualValues(t, 0, countBlobs(t, buf))
}

func TestSaveAsPackageBlobDoesNotRetryAfterWritingContent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	buf := hashedBuffer(t, "race test blob: violation after the content was written")
	blobInserts := 0
	installRaceHook(t, failInsertWithViolation("package_file", "sha256_"+packages_service.NewPackageBlob(buf).HashSHA256, "package_blob", &blobInserts))

	_, err := SaveAsPackageBlob(t.Context(), buf, blobCreationInfo(user2, "race-blob-no-retry"))
	require.ErrorContains(t, err, errInjected.Error())

	// A retry would read the consumed buf and replace the content file with an empty one.
	assert.Equal(t, 1, blobInserts)
	assert.False(t, contentExists(buf))
}

const (
	raceLayerBase64    = "H4sIAAAJbogA/2IYBaNgFIxYAAgAAP//Lq+17wAEAAA="
	raceConfigContent  = `{"architecture":"amd64","config":{"Env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],"Cmd":["/true"],"ArgsEscaped":true,"Image":"sha256:9bd8b88dc68b80cffe126cc820e4b52c6e558eb3b37680bfee8e5f3ed7b8c257"},"container":"b89fe92a887d55c0961f02bdfbfd8ac3ddf66167db374770d2d9e9fab3311510","container_config":{"Hostname":"b89fe92a887d","Env":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],"Cmd":["/bin/sh","-c","#(nop) ","CMD [\"/true\"]"],"ArgsEscaped":true,"Image":"sha256:9bd8b88dc68b80cffe126cc820e4b52c6e558eb3b37680bfee8e5f3ed7b8c257"},"created":"2022-01-01T00:00:00.000000000Z","docker_version":"20.10.12","history":[{"created":"2022-01-01T00:00:00.000000000Z","created_by":"/bin/sh -c #(nop) COPY file:0e7589b0c800daaf6fa460d2677101e4676dd9491980210cb345480e513f3602 in /true "},{"created":"2022-01-01T00:00:00.000000001Z","created_by":"/bin/sh -c #(nop)  CMD [\"/true\"]","empty_layer":true}],"os":"linux","rootfs":{"type":"layers","diff_ids":["sha256:0ff3b91bdf21ecdf2f2f3d4372c2098a14dbe06cd678e8f0a85fd4902d00e2e2"]}}`
	raceImageManifest  = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":"sha256:4607e093bec406eaadb6f3a340f63400c9d3a7038680744c406903766b938f0d","size":1069},"layers":[{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","digest":"sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4","size":32}]}`
	raceManifestType   = "application/vnd.docker.distribution.manifest.v2+json"
	raceManifestDigest = "sha256:4f10484d1c1bb13e3956b4de1cd42db8e0f14a75be1617b60f2de3cd59c803c6"
)

func pushImageBlobs(t *testing.T, owner *user_model.User, image string) {
	t.Helper()
	layer, err := base64.StdEncoding.DecodeString(raceLayerBase64)
	require.NoError(t, err)
	for _, content := range []string{string(layer), raceConfigContent} {
		_, err := SaveAsPackageBlob(t.Context(), hashedBuffer(t, content), blobCreationInfo(owner, image))
		require.NoError(t, err)
	}
}

func putManifest(t *testing.T, owner *user_model.User, image, tag, manifest string) (string, error) {
	t.Helper()
	mci, err := NewManifestCreationInfo(owner, owner, raceManifestType, image, tag)
	require.NoError(t, err)
	return ProcessManifest(t.Context(), *mci, hashedBuffer(t, manifest))
}

func countVersions(t *testing.T, image, tag string) int64 {
	t.Helper()
	n, err := db.GetEngine(t.Context()).
		Table("package_version").
		Join("INNER", "package", "package.id = package_version.package_id").
		Where("package.lower_name = ? AND package_version.lower_version = ?", image, tag).
		Count()
	require.NoError(t, err)
	return n
}

func TestProcessManifestLostVersionRace(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	image := "race-manifest-version"
	pushImageBlobs(t, user2, image)
	digest, err := putManifest(t, user2, image, "latest", raceImageManifest)
	require.NoError(t, err)
	require.Equal(t, raceManifestDigest, digest)

	hook := installRaceHook(t, missFirstLookup("FROM `package_version` WHERE lower_version=? AND package_id=?", rewriteVersion))

	digest, err = putManifest(t, user2, image, "latest", raceImageManifest)
	require.NoError(t, err)
	assert.Equal(t, raceManifestDigest, digest)

	assert.Equal(t, 1, hook.violations(), "the loser must have lost the insert")
	assert.EqualValues(t, 1, countVersions(t, image, "latest"))
	pfd, err := GetLocalManifest(t.Context(), user2.ID, image, "latest")
	require.NoError(t, err)
	assert.Equal(t, raceManifestDigest, "sha256:"+pfd.Blob.HashSHA256)
}

func TestProcessManifestLostBlobRaceThenFailureKeepsWinnersContent(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	image := "race-manifest-blob"
	pushImageBlobs(t, user2, image)
	_, err := putManifest(t, user2, image, "winner", raceImageManifest)
	require.NoError(t, err)
	manifestBuf := hashedBuffer(t, raceImageManifest)
	require.True(t, contentExists(manifestBuf))

	hook := installRaceHook(t, chainRules(
		missFirstLookup("FROM `package_blob` WHERE hash_md5=?", rewriteSize),
		failInsert("package_file", container_model.ManifestFilename),
	))

	_, err = putManifest(t, user2, image, "loser", raceImageManifest)
	require.ErrorIs(t, err, errInjected)

	assert.Equal(t, 1, hook.violations(), "the loser must have lost the insert")
	assert.EqualValues(t, 1, countBlobs(t, manifestBuf))
	assert.True(t, contentExists(manifestBuf), "the loser's cleanup deleted the winner's content")
}

func TestProcessManifestLostOverwriteRaceOnExistingTag(t *testing.T) {
	require.NoError(t, unittest.PrepareTestDatabase())
	user2 := unittest.AssertExistsAndLoadBean(t, &user_model.User{ID: 2})

	image := "race-manifest-overwrite"
	pushImageBlobs(t, user2, image)
	// The winner already replaced the tag's old version with this one.
	_, err := putManifest(t, user2, image, "latest", raceImageManifest)
	require.NoError(t, err)
	// Stands in for the old version the loser found before the winner committed.
	// Deleting it does not free the tag, as deleting the winner-deleted old version on PostgreSQL does not.
	_, err = putManifest(t, user2, image, "decoy", raceImageManifest)
	require.NoError(t, err)

	installRaceHook(t, missFirstLookup("FROM `package_version` WHERE lower_version=? AND package_id=?", func(arg any) (any, bool) {
		version, ok := arg.(string)
		return "decoy", ok && version == "latest"
	}))

	digest, err := putManifest(t, user2, image, "latest", raceImageManifest)
	require.NoError(t, err)
	assert.Equal(t, raceManifestDigest, digest)
	assert.EqualValues(t, 1, countVersions(t, image, "latest"))
}
