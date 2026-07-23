// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package integration

import (
	"fmt"
	"net/http"
	"testing"

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
