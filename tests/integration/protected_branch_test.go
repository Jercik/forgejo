// Copyright 2024 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package integration

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	git_model "forgejo.org/models/git"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unittest"
	"forgejo.org/tests"

	"github.com/stretchr/testify/assert"
)

func TestProtectedBranch_AdminEnforcement(t *testing.T) {
	onApplicationRun(t, func(t *testing.T, u *url.URL) {
		session := loginUser(t, "user1")
		testRepoFork(t, session, "user2", "repo1", "user1", "repo1")
		testEditFileToNewBranch(t, session, "user1", "repo1", "master", "add-readme", "README.md", "WIP")
		repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{OwnerID: 1, Name: "repo1"})

		req := NewRequestWithValues(t, "POST", "user1/repo1/compare/master...add-readme", map[string]string{
			"title": "pull request",
		})
		session.MakeRequest(t, req, http.StatusOK)

		t.Run("No protected branch", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()

			req = NewRequest(t, "GET", "/user1/repo1/pulls/1")
			resp := session.MakeRequest(t, req, http.StatusOK)
			doc := NewHTMLParser(t, resp.Body)

			text := strings.TrimSpace(doc.doc.Find(".merge-section").Text())
			assert.Contains(t, text, "This pull request can be merged automatically.")
			assert.Contains(t, text, "'canMergeNow':  true")
		})

		t.Run("Without admin enforcement", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()

			req := NewRequestWithValues(t, "POST", "/user1/repo1/settings/branches/edit", map[string]string{
				"rule_name":          "master",
				"required_approvals": "1",
			})
			session.MakeRequest(t, req, http.StatusSeeOther)

			req = NewRequest(t, "GET", "/user1/repo1/pulls/1")
			resp := session.MakeRequest(t, req, http.StatusOK)
			doc := NewHTMLParser(t, resp.Body)

			text := strings.TrimSpace(doc.doc.Find(".merge-section").Text())
			assert.Contains(t, text, "This pull request doesn't have enough approvals yet. 0 of 1 approvals granted.")
			assert.Contains(t, text, "'canMergeNow':  true")
		})

		t.Run("With admin enforcement", func(t *testing.T) {
			defer tests.PrintCurrentTest(t)()

			protectedBranch := unittest.AssertExistsAndLoadBean(t, &git_model.ProtectedBranch{RuleName: "master", RepoID: repo.ID})
			req := NewRequestWithValues(t, "POST", "/user1/repo1/settings/branches/edit", map[string]string{
				"rule_name":          "master",
				"rule_id":            strconv.FormatInt(protectedBranch.ID, 10),
				"required_approvals": "1",
				"apply_to_admins":    "true",
			})
			session.MakeRequest(t, req, http.StatusSeeOther)

			req = NewRequest(t, "GET", "/user1/repo1/pulls/1")
			resp := session.MakeRequest(t, req, http.StatusOK)
			doc := NewHTMLParser(t, resp.Body)

			text := strings.TrimSpace(doc.doc.Find(".merge-section").Text())
			assert.Contains(t, text, "This pull request doesn't have enough approvals yet. 0 of 1 approvals granted.")
			assert.Contains(t, text, "'canMergeNow':  false")
		})
	})
}

func TestProtectedBranch_RejectsUnusableStatusCheckPattern(t *testing.T) {
	defer tests.PrepareTestEnv(t)()

	session := loginUser(t, "user2")
	rule := func(contexts string) map[string]string {
		return map[string]string{
			"rule_name":             "release/*",
			"enable_status_check":   "true",
			"status_check_contexts": contexts,
		}
	}

	for _, pattern := range []string{"lint [broken", "a{,"} {
		req := NewRequestWithValues(t, "POST", "/user2/repo1/settings/branches/edit", rule("test1\n"+pattern))
		resp := session.MakeRequest(t, req, http.StatusSeeOther)
		assert.Contains(t, resp.Header().Get("Location"), "/settings/branches/edit?rule_name=release")
		unittest.AssertNotExistsBean(t, &git_model.ProtectedBranch{RepoID: 1, RuleName: "release/*"})
	}

	req := NewRequestWithValues(t, "POST", "/user2/repo1/settings/branches/edit", rule("test1\nlint {a,b}"))
	session.MakeRequest(t, req, http.StatusSeeOther)
	unittest.AssertExistsAndLoadBean(t, &git_model.ProtectedBranch{RepoID: 1, RuleName: "release/*"})
}
