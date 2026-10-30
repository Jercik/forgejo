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
	if m, ok := g.(match.Matcher); ok && hasEmptyRowElement(m) {
		return nil, errors.New(`has an unclosed "{" or an empty "{...}" alternative that the glob library cannot match; for an optional part, write the empty alternative first, as in "{,_target}"`)
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

// hasEmptyRowElement reports whether m contains a fixed-length sequence of
// matchers (match.Row) with an element of length zero. gobwas/glob v0.2.3
// match/row.go matchAll slices the subject by the summed element lengths, so
// once the earlier elements have consumed all of it, a zero-length element
// slices past its end and Match panics. Elements before the last one reach this
// state too: AnyOf.Len() reports the length of a later alternative when the
// first is unbounded, and Row.Index measures the subject in bytes, not
// characters. An empty alternation, `{}` or `{,}`, compiles to such an element,
// and so does an unclosed `{`, which gobwas/glob accepts as an alternation that
// runs to the end of the pattern. A sequence with a zero-length element can
// only report no match or panic, so rejecting it loses no usable pattern.
func hasEmptyRowElement(m match.Matcher) bool {
	switch v := m.(type) {
	case match.Row:
		for _, element := range v.Matchers {
			if element.Len() == 0 {
				return true
			}
		}
		return anyHasEmptyRowElement(v.Matchers...)
	case match.AnyOf:
		return anyHasEmptyRowElement(v.Matchers...)
	case match.EveryOf:
		return anyHasEmptyRowElement(v.Matchers...)
	case match.BTree:
		return anyHasEmptyRowElement(v.Value, v.Left, v.Right)
	}
	return false
}

func anyHasEmptyRowElement(matchers ...match.Matcher) bool {
	for _, m := range matchers {
		if m != nil && hasEmptyRowElement(m) {
			return true
		}
	}
	return false
}
