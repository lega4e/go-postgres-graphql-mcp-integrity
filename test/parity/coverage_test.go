package parity_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// executeStep matches the godog step the milestone suites run a query with. The
// query text is the first quoted string; a trailing `with variable "n" bound to
// "Alice"` follows it and is deliberately not captured — the catalogue is keyed
// on the query, and the variable bindings ride on its entries.
var executeStep = regexp.MustCompile(`I compile and execute "([^"]*)"`)

// TestCatalogueCoversEveryMilestoneQuery is the guard that keeps the catalogue
// complete (design D6).
//
// The exit criterion is "every prior milestone's query scenarios re-run under
// SQL-side shaping". Restating that list in prose would be wrong within one
// milestone, so instead the feature files are read and any query absent from the
// catalogue fails the build by name. A later milestone that adds a query has to
// touch the catalogue to make CI green again.
//
// It reads the feature files as text, which a step phrased slightly differently
// would slip past. That is a known limit and it fails safe in one direction
// only: a missed query is not *tested*, never wrongly *passed*.
func TestCatalogueCoversEveryMilestoneQuery(t *testing.T) {
	features, err := filepath.Glob(filepath.Join("..", "m*", "features", "*.feature"))
	require.NoError(t, err)
	require.NotEmpty(t, features, "found no milestone feature files to check against")

	covered := map[string]bool{}
	for _, sc := range catalogue {
		covered[sc.query] = true
	}

	// Where each executed query lives, so a failure names the file to look in.
	sources := map[string][]string{}
	for _, path := range features {
		content, err := os.ReadFile(path) //nolint:gosec // a path this test globbed
		require.NoError(t, err)
		for _, m := range executeStep.FindAllStringSubmatch(string(content), -1) {
			sources[m[1]] = append(sources[m[1]], path)
		}
	}
	require.NotEmpty(t, sources, "found no executed queries in the feature files")

	var missing []string
	for query, paths := range sources {
		if !covered[query] {
			missing = append(missing, query+"  ("+strings.Join(uniq(paths), ", ")+")")
		}
	}
	sort.Strings(missing)

	assert.Empty(t, missing,
		"these queries are executed by a milestone suite but are not in the parity catalogue, "+
			"so they are not proven byte-identical under SQL-side shaping:\n  %s",
		strings.Join(missing, "\n  "))
}

// TestCatalogueCoversM7 covers the one milestone the text scan cannot reach.
//
// M7 has no feature file — it drives exec.Query from Go — so its entries are
// registered in the catalogue explicitly and the count is asserted here instead.
// If M7 grows a third query through exec.Query, this fails and the catalogue has
// to be updated to match (design D6).
func TestCatalogueCoversM7(t *testing.T) {
	const wantM7 = 2

	var got []string
	for _, sc := range catalogue {
		if sc.milestone == "M7" {
			got = append(got, sc.query)
		}
	}
	assert.Len(t, got, wantM7,
		"M7 runs %d queries through exec.Query (test/m7/m7_test.go, the e.run calls); "+
			"the catalogue carries %d", wantM7, len(got))
}

// TestCatalogueEntriesAreWellFormed catches the mistakes that would make an
// entry silently do nothing: a duplicate name, no world, or an empty query.
func TestCatalogueEntriesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, sc := range catalogue {
		assert.NotEmpty(t, sc.query, "entry %q has no query", sc.name)
		assert.NotEmpty(t, sc.milestone, "entry %q names no milestone", sc.name)
		require.NotNil(t, sc.world, "entry %q has no world", sc.name)
		assert.False(t, seen[sc.name], "two catalogue entries are named %q", sc.name)
		seen[sc.name] = true
	}
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
