// Copyright 2026 The Forgejo Authors. All rights reserved.
// SPDX-License-Identifier: GPL-3.0-or-later

package git

import (
	"errors"
	"fmt"

	"github.com/gobwas/glob"
	"github.com/gobwas/glob/match"
)

// StatusCheckPattern is a compiled entry of ProtectedBranch.StatusCheckContexts:
// a glob that is matched against the contexts of commit statuses.
type StatusCheckPattern struct {
	pattern string
	glob    glob.Glob
}

// CompileStatusCheckPattern compiles a required status check pattern. It fails
// for a pattern that gobwas/glob cannot compile, such as an unclosed `[`, and
// for one that compiles into a matcher that panics when it matches, such as
// `a{,`.
func CompileStatusCheckPattern(pattern string) (*StatusCheckPattern, error) {
	g, err := glob.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if m, ok := g.(match.Matcher); ok && hasEmptyRowTail(m) {
		return nil, errors.New(`its matcher would panic, check for an unclosed or empty "{...}"`)
	}
	return &StatusCheckPattern{pattern: pattern, glob: g}, nil
}

// Match reports whether the context of a commit status matches the pattern.
// A panic in the glob library is returned as an error instead of unwinding the
// caller, so that a pattern that slipped past CompileStatusCheckPattern can
// still be handled as a failed check.
func (p *StatusCheckPattern) Match(statusContext string) (matched bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			matched, err = false, fmt.Errorf("status check pattern %q panicked on context %q: %v", p.pattern, statusContext, r)
		}
	}()
	return p.glob.Match(statusContext), nil
}

// hasEmptyRowTail reports whether m contains a fixed-length sequence of
// matchers (match.Row) that ends in a matcher of length zero. Once the earlier
// matchers have consumed all of the subject, the sequence slices past its end
// (gobwas/glob v0.2.3 match/row.go matchAll), so Match panics for exactly the
// subjects the sequence would otherwise accept. An empty alternation, `{}` or
// `{,}`, compiles to such a matcher, and so does an unclosed `{`, which
// gobwas/glob accepts as an alternation that runs to the end of the pattern.
func hasEmptyRowTail(m match.Matcher) bool {
	switch v := m.(type) {
	case match.Row:
		if n := len(v.Matchers); n > 0 && v.Matchers[n-1].Len() == 0 {
			return true
		}
		return anyHasEmptyRowTail(v.Matchers...)
	case match.AnyOf:
		return anyHasEmptyRowTail(v.Matchers...)
	case match.EveryOf:
		return anyHasEmptyRowTail(v.Matchers...)
	case match.BTree:
		return anyHasEmptyRowTail(v.Value, v.Left, v.Right)
	}
	return false
}

func anyHasEmptyRowTail(matchers ...match.Matcher) bool {
	for _, m := range matchers {
		if m != nil && hasEmptyRowTail(m) {
			return true
		}
	}
	return false
}
