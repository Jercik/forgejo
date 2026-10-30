// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompileStatusCheckPattern(t *testing.T) {
	valid := []string{
		"commit-msg / commitlint (pull_request)",
		"Build*",
		"lint [strict]",
		"Lint / {yamllint,yaml-lint} (pull_request)",
		"Validate / contract (pull_request{,_target})",
		"{a,b}",
		"Build{, *} (push)",
		"a{b,c}d",
	}
	for _, pattern := range valid {
		t.Run("valid "+pattern, func(t *testing.T) {
			_, err := CompileStatusCheckPattern(pattern)
			assert.NoError(t, err)
		})
	}

	invalid := []string{
		"lint [broken",
		"[",
		"[]",
		"a{,",
		"a{",
		"a{}",
		"a{,}",
		"{a*,a",
		"a{}b",
		"{*,}{}a",
		"lint{ *,}{-*,} (push)",
		"*éx{}?*",
		"*{?[!x]}{}?*",
		"*ł / lint{}x",
	}
	for _, pattern := range invalid {
		t.Run("invalid "+pattern, func(t *testing.T) {
			_, err := CompileStatusCheckPattern(pattern)
			assert.Error(t, err)
		})
	}
}

func TestStatusCheckPatternMatch(t *testing.T) {
	p, err := CompileStatusCheckPattern("Validate / contract (pull_request{,_target})")
	require.NoError(t, err)

	matched, err := p.Match("Validate / contract (pull_request)")
	require.NoError(t, err)
	assert.True(t, matched)

	matched, err = p.Match("Validate / contract (pull_request_target)")
	require.NoError(t, err)
	assert.True(t, matched)

	matched, err = p.Match("Validate / contract (push)")
	require.NoError(t, err)
	assert.False(t, matched)
}

type panickingGlob struct{}

func (panickingGlob) Match(string) bool { panic("boom") }

func TestStatusCheckPatternMatchReturnsPanicAsError(t *testing.T) {
	p := &StatusCheckPattern{pattern: "a{,", glob: panickingGlob{}}

	matched, err := p.Match("a")
	assert.False(t, matched)
	assert.ErrorContains(t, err, `status check pattern "a{," panicked on context "a": boom`)
}

// gobwas/glob compiles some patterns into matchers that panic for particular
// subjects. Every pattern that CompileStatusCheckPattern accepts must match
// every subject without panicking.
func TestCompileStatusCheckPatternRejectsEveryPanickingPattern(t *testing.T) {
	patterns := allStrings("a{},*?[]", 4)
	subjects := allStrings("a,{}é", 4)

	for _, pattern := range patterns {
		p, err := CompileStatusCheckPattern(pattern)
		if err != nil {
			continue
		}
		for _, subject := range subjects {
			if _, err := p.Match(subject); err != nil {
				t.Errorf("pattern %q is accepted but fails on %q: %v", pattern, subject, err)
				break
			}
		}
	}
}

// allStrings returns every string of up to maxLen characters from alphabet,
// including the empty string.
func allStrings(alphabet string, maxLen int) []string {
	all := []string{""}
	last := []string{""}
	for range maxLen {
		var next []string
		for _, prefix := range last {
			for _, char := range alphabet {
				next = append(next, prefix+string(char))
			}
		}
		all = append(all, next...)
		last = next
	}
	return all
}
