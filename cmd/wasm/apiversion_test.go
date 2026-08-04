package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The page and the module carry the same version number in two files, and the
// staleness check in docs/src/main.js exists to refuse a mismatched pair. That
// check works — it is what stopped a page built for v5 from silently running
// against a module exporting v6, which is exactly the failure it was written
// for.
//
// What nothing enforced was that the two numbers change *together*. Bumping
// `apiVersion` in one commit and `REQUIRED_API_VERSION` in the next leaves an
// interval where every build is a mismatched pair — green in CI, because
// nothing in CI loads the page, and broken for anyone who opens a PR preview
// in the meantime. That is not a hypothetical: it happened on this branch, and
// a reader found it before any test did.
//
// So the invariant is asserted here rather than left to whoever is editing.
// A split bump now fails on the commit that splits it.

// apiVersionRe matches the Go constant the module exports.
var apiVersionRe = regexp.MustCompile(`(?m)^const apiVersion = (\d+)$`)

// requiredVersionRe matches the JavaScript constant the page requires.
var requiredVersionRe = regexp.MustCompile(`(?m)^const REQUIRED_API_VERSION = (\d+)$`)

// main.go is read as a file rather than referenced as a symbol because it is
// behind `//go:build js && wasm` and so is not compiled into this test's
// binary. That is also why the constant's declaration has to keep its exact
// one-line shape — which the failure messages below say out loud, since a
// reformatting that breaks the match would otherwise look like a missing bump.
func extract(t *testing.T, path string, re *regexp.Regexp, what string) string {
	t.Helper()
	src, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)
	m := re.FindSubmatch(src)
	require.NotNil(t, m,
		"could not find %s in %s. If it was reformatted or renamed, update %s "+
			"so the two versions stay checked against each other.", what, path, "TestAPIVersionsAgree")
	return string(m[1])
}

func TestAPIVersionsAgree(t *testing.T) {
	module := extract(t, "main.go", apiVersionRe, "`const apiVersion = N`")
	page := extract(t, "../../docs/src/main.js", requiredVersionRe,
		"`const REQUIRED_API_VERSION = N`")

	assert.Equal(t, module, page,
		"cmd/wasm exports API v%s but docs/src/main.js requires v%s.\n"+
			"These must change in the same commit. A commit that moves only one of "+
			"them ships a page and a module that refuse each other — which CI cannot "+
			"see, because nothing in CI loads the page, but every PR preview built "+
			"from that commit is broken for whoever opens it.",
		module, page)
}
