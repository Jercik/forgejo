// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package integration

import (
	"fmt"
	"net/http"
	"testing"

	actions_model "forgejo.org/models/actions"
	auth_model "forgejo.org/models/auth"
	"forgejo.org/models/db"
	issues_model "forgejo.org/models/issues"
	repo_model "forgejo.org/models/repo"
	"forgejo.org/models/unittest"
	api "forgejo.org/modules/structs"
	"forgejo.org/tests"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIPullReviewResolve(t *testing.T) {
	defer tests.PrepareTestEnv(t)()
	pullIssue := unittest.AssertExistsAndLoadBean(t, &issues_model.Issue{ID: 3})
	require.NoError(t, pullIssue.LoadAttributes(db.DefaultContext))
	repo := unittest.AssertExistsAndLoadBean(t, &repo_model.Repository{ID: pullIssue.RepoID})

	session := loginUser(t, "user2")
	token := getTokenForLoggedInUser(t, session, auth_model.AccessTokenScopeWriteRepository)

	// create a review with a code comment
	var review api.PullReview
	req := NewRequestWithJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", repo.OwnerName, repo.Name, pullIssue.Index), &api.CreatePullReviewOptions{
		Body:  "review with a code comment",
		Event: "COMMENT",
		Comments: []api.CreatePullReviewComment{
			{
				Path:       "README.md",
				Body:       "please resolve me",
				NewLineNum: 1,
			},
		},
	}).AddTokenAuth(token)
	resp := MakeRequest(t, req, http.StatusOK)
	DecodeJSON(t, resp, &review)
	assert.Equal(t, 1, review.CodeCommentsCount)

	commentsURL := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews/%d/comments", repo.OwnerName, repo.Name, pullIssue.Index, review.ID)

	// fetch the code comment id
	req = NewRequest(t, http.MethodGet, commentsURL).AddTokenAuth(token)
	resp = MakeRequest(t, req, http.StatusOK)
	var reviewComments []*api.PullReviewComment
	DecodeJSON(t, resp, &reviewComments)
	require.Len(t, reviewComments, 1)
	commentID := reviewComments[0].ID
	assert.Nil(t, reviewComments[0].Resolver)

	resolutionURL := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews/%d/comments/%d/resolution", repo.OwnerName, repo.Name, pullIssue.Index, review.ID, commentID)

	t.Run("Actions token", func(t *testing.T) {
		task := unittest.AssertExistsAndLoadBean(t, &actions_model.ActionTask{ID: 47})
		task.RepoID = repo.ID
		task.OwnerID = repo.OwnerID
		task.IsForkPullRequest = false
		task.GenerateToken()
		require.NoError(t, actions_model.UpdateTask(t.Context(), task, "repo_id", "owner_id", "is_fork_pull_request", "token_hash", "token_salt", "token_last_eight"))

		resp := MakeRequest(t, NewRequest(t, http.MethodPost, resolutionURL).AddTokenAuth(task.Token), http.StatusOK)
		var comment api.PullReviewComment
		DecodeJSON(t, resp, &comment)
		require.NotNil(t, comment.Resolver)
		assert.Equal(t, "forgejo-actions", comment.Resolver.UserName)
		persisted := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: commentID})
		assert.EqualValues(t, -2, persisted.ResolveDoerID)

		resp = MakeRequest(t, NewRequest(t, http.MethodDelete, resolutionURL).AddTokenAuth(task.Token), http.StatusOK)
		DecodeJSON(t, resp, &comment)
		assert.Nil(t, comment.Resolver)
		persisted = unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: commentID})
		assert.Zero(t, persisted.ResolveDoerID)

		var botReview api.PullReview
		resp = MakeRequest(t, NewRequestWithJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", repo.OwnerName, repo.Name, pullIssue.Index), &api.CreatePullReviewOptions{
			Event: "COMMENT",
			Comments: []api.CreatePullReviewComment{{
				Path:       "README.md",
				Body:       "Actions review comment",
				NewLineNum: 1,
			}},
		}).AddTokenAuth(task.Token), http.StatusOK)
		DecodeJSON(t, resp, &botReview)
		botComment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ReviewID: botReview.ID, Type: issues_model.CommentTypeCode})
		assert.EqualValues(t, -2, botComment.PosterID)
		botResolutionURL := fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews/%d/comments/%d/resolution", repo.OwnerName, repo.Name, pullIssue.Index, botReview.ID, botComment.ID)
		resp = MakeRequest(t, NewRequest(t, http.MethodPost, botResolutionURL).AddTokenAuth(task.Token), http.StatusOK)
		DecodeJSON(t, resp, &comment)
		require.NotNil(t, comment.Resolver)
		assert.Equal(t, "forgejo-actions", comment.Resolver.UserName)
		botComment = unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: botComment.ID})
		assert.EqualValues(t, -2, botComment.ResolveDoerID)
		resp = MakeRequest(t, NewRequest(t, http.MethodDelete, botResolutionURL).AddTokenAuth(task.Token), http.StatusOK)
		DecodeJSON(t, resp, &comment)
		assert.Nil(t, comment.Resolver)
		botComment = unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: botComment.ID})
		assert.Zero(t, botComment.ResolveDoerID)

		task.IsForkPullRequest = true
		require.NoError(t, actions_model.UpdateTask(t.Context(), task, "is_fork_pull_request"))
		MakeRequest(t, NewRequest(t, http.MethodPost, resolutionURL).AddTokenAuth(task.Token), http.StatusForbidden)
		MakeRequest(t, NewRequest(t, http.MethodDelete, resolutionURL).AddTokenAuth(task.Token), http.StatusForbidden)

		task.IsForkPullRequest = false
		task.RepoID = 2
		require.NoError(t, actions_model.UpdateTask(t.Context(), task, "repo_id", "is_fork_pull_request"))
		MakeRequest(t, NewRequest(t, http.MethodPost, resolutionURL).AddTokenAuth(task.Token), http.StatusNotFound)
		MakeRequest(t, NewRequest(t, http.MethodDelete, resolutionURL).AddTokenAuth(task.Token), http.StatusNotFound)
		persisted = unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: commentID})
		assert.Zero(t, persisted.ResolveDoerID)
	})

	// resolve the conversation
	{
		req = NewRequest(t, http.MethodPost, resolutionURL).AddTokenAuth(token)
		resp = MakeRequest(t, req, http.StatusOK)
		var comment api.PullReviewComment
		DecodeJSON(t, resp, &comment)
		require.NotNil(t, comment.Resolver)
		assert.Equal(t, "user2", comment.Resolver.UserName)
	}

	// confirm the resolution is persisted
	{
		comment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: commentID})
		assert.EqualValues(t, 2, comment.ResolveDoerID)
	}

	// the user-visible read path reports the resolver
	{
		req = NewRequest(t, http.MethodGet, commentsURL).AddTokenAuth(token)
		resp = MakeRequest(t, req, http.StatusOK)
		var comments []*api.PullReviewComment
		DecodeJSON(t, resp, &comments)
		require.Len(t, comments, 1)
		require.NotNil(t, comments[0].Resolver)
		assert.Equal(t, "user2", comments[0].Resolver.UserName)
	}

	// re-resolving as a different permitted user must not overwrite the original
	// resolver: the response surfaces the real resolver (user2) and the DB is untouched
	{
		adminSession := loginUser(t, "user1")
		adminToken := getTokenForLoggedInUser(t, adminSession, auth_model.AccessTokenScopeWriteRepository)
		req = NewRequest(t, http.MethodPost, resolutionURL).AddTokenAuth(adminToken)
		resp = MakeRequest(t, req, http.StatusOK)
		var comment api.PullReviewComment
		DecodeJSON(t, resp, &comment)
		require.NotNil(t, comment.Resolver)
		assert.Equal(t, "user2", comment.Resolver.UserName)

		persisted := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: commentID})
		assert.EqualValues(t, 2, persisted.ResolveDoerID)
	}

	// unresolve the conversation
	{
		req = NewRequest(t, http.MethodDelete, resolutionURL).AddTokenAuth(token)
		resp = MakeRequest(t, req, http.StatusOK)
		var comment api.PullReviewComment
		DecodeJSON(t, resp, &comment)
		assert.Nil(t, comment.Resolver)
	}

	// confirm the unresolve is persisted
	{
		comment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ID: commentID})
		assert.EqualValues(t, 0, comment.ResolveDoerID)
	}

	// the user-visible read path no longer reports a resolver
	{
		req = NewRequest(t, http.MethodGet, commentsURL).AddTokenAuth(token)
		resp = MakeRequest(t, req, http.StatusOK)
		var comments []*api.PullReviewComment
		DecodeJSON(t, resp, &comments)
		require.Len(t, comments, 1)
		assert.Nil(t, comments[0].Resolver)
	}

	// a user with only read access, who is neither the poster nor an official reviewer, gets 403
	{
		readSession := loginUser(t, "user8")
		readToken := getTokenForLoggedInUser(t, readSession, auth_model.AccessTokenScopeWriteRepository)
		req = NewRequest(t, http.MethodPost, resolutionURL).AddTokenAuth(readToken)
		MakeRequest(t, req, http.StatusForbidden)
	}

	// a non-code comment gets 404 even when it belongs to the review: the review-body
	// comment shares this review's id, so only the comment-type guard can reject it
	{
		reviewComment := unittest.AssertExistsAndLoadBean(t, &issues_model.Comment{ReviewID: review.ID, Type: issues_model.CommentTypeReview})
		req = NewRequestf(t, http.MethodPost, "/api/v1/repos/%s/%s/pulls/%d/reviews/%d/comments/%d/resolution", repo.OwnerName, repo.Name, pullIssue.Index, review.ID, reviewComment.ID).
			AddTokenAuth(token)
		MakeRequest(t, req, http.StatusNotFound)
	}

	// a valid code comment addressed through a different review id on the same PR gets 404
	{
		var otherReview api.PullReview
		req = NewRequestWithJSON(t, http.MethodPost, fmt.Sprintf("/api/v1/repos/%s/%s/pulls/%d/reviews", repo.OwnerName, repo.Name, pullIssue.Index), &api.CreatePullReviewOptions{
			Body:  "another review",
			Event: "COMMENT",
		}).AddTokenAuth(token)
		resp = MakeRequest(t, req, http.StatusOK)
		DecodeJSON(t, resp, &otherReview)

		req = NewRequestf(t, http.MethodPost, "/api/v1/repos/%s/%s/pulls/%d/reviews/%d/comments/%d/resolution", repo.OwnerName, repo.Name, pullIssue.Index, otherReview.ID, commentID).
			AddTokenAuth(token)
		MakeRequest(t, req, http.StatusNotFound)
	}

	// an unauthenticated request is rejected the same way sibling reqToken routes are
	{
		req = NewRequest(t, http.MethodPost, resolutionURL)
		MakeRequest(t, req, http.StatusUnauthorized)
	}

	// resolving on an archived repo is rejected the same way sibling mustNotBeArchived routes are
	{
		require.NoError(t, repo_model.SetArchiveRepoState(db.DefaultContext, repo, true))
		req = NewRequest(t, http.MethodPost, resolutionURL).AddTokenAuth(token)
		MakeRequest(t, req, http.StatusLocked)
		require.NoError(t, repo_model.SetArchiveRepoState(db.DefaultContext, repo, false))
	}
}
