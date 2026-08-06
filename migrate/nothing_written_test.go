package migrate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lega4e/gopgql/schema"
)

// TestNoOpWithAnUncreatedOwnedTableIsRefused covers the second half of item A:
// "already up to date" has to be true when it is said.
//
// The directory here holds a history that creates one of the two owned tables
// and not the other, and nothing about the SDL has changed since. Before this,
// any generation that planned nothing printed "already up to date" and exited 0,
// which is indistinguishable from success — and is exactly what gopgql#53 looked
// like from the outside.
func TestNoOpWithAnUncreatedOwnedTableIsRefused(t *testing.T) {
	desired := build(t, twoSchemaSDL)

	// A prior state that creates agentiq.event but not agentiq.session, and whose
	// graph is already the desired one — so nothing is planned.
	prior := &schema.Schema{GraphName: desired.GraphName}
	for _, vt := range desired.VertexTables {
		if vt.Name == "session" {
			vt.Columns = nil // in the graph, never created
		}
		prior.VertexTables = append(prior.VertexTables, vt)
	}
	prior.EdgeTables = desired.EdgeTables
	prior.Indexes = desired.Indexes

	err := checkNothingOwedIsMissing(prior, desired, Halves{})
	require.ErrorIs(t, err, ErrNothingWritten)
	assert.Contains(t, err.Error(), "agentiq.session",
		"the refusal names the table that would have been missing")
	assert.NotContains(t, err.Error(), "dbos.operation_outputs",
		"a table gopgql does not own is not owed")
}

// The check must stay quiet where the history really does hold everything, or
// every second run of every schema fails.
func TestNoOpIsAcceptedWhenTheHistoryHoldsEverything(t *testing.T) {
	desired := build(t, twoSchemaSDL)
	assert.NoError(t, checkNothingOwedIsMissing(desired, desired, Halves{}))

	// A --no-tables directory folds every table without columns, so the evidence
	// this check reads is absent by construction and it must not be read.
	assert.NoError(t, checkNothingOwedIsMissing(nil, desired, Halves{NoTables: true}))
}
