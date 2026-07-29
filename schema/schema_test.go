package schema_test

import (
	"testing"

	"github.com/lega4e/gopgql/schema"
	"github.com/stretchr/testify/assert"
)

// TestClassifyConstraint pins the inverse of the four naming rules.
//
// It is worth its own test because a DROP CONSTRAINT statement carries nothing
// but a name: this function is the entire channel through which the fold learns
// what a drop dropped. Get one case wrong and prior state silently keeps a
// constraint the database no longer has — or loses one it does.
func TestClassifyConstraint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		role    schema.ConstraintRole
		column  string
		ordinal int
	}{
		{"natural key", "persons_key", schema.RoleNaturalKey, "", 0},
		{"column unique", "persons_email_key", schema.RoleColumnUnique, "email", 0},
		{"column check", "persons_age_check", schema.RoleColumnCheck, "age", 0},
		{"table check", "persons_check_1", schema.RoleTableCheck, "", 1},
		{"table check, two digits", "persons_check_12", schema.RoleTableCheck, "", 12},
		{"another table's constraint", "companies_key", schema.RoleUnknown, "", 0},
		{"nothing gopgql emits", "persons_pkey", schema.RoleUnknown, "", 0},
		{"underscored column", "persons_email_address_key", schema.RoleColumnUnique, "email_address", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role, column, ordinal := schema.ClassifyConstraint("persons", tc.in)
			assert.Equal(t, tc.role, role)
			assert.Equal(t, tc.column, column)
			assert.Equal(t, tc.ordinal, ordinal)
		})
	}
}

// TestClassifyConstraintInvertsTheNamers states the property the two directions
// have to satisfy together, rather than trusting that two hand-written tables
// agree.
func TestClassifyConstraintInvertsTheNamers(t *testing.T) {
	const table = "persons"

	role, _, _ := schema.ClassifyConstraint(table, schema.NaturalKeyConstraintName(table))
	assert.Equal(t, schema.RoleNaturalKey, role)

	role, col, _ := schema.ClassifyConstraint(table, schema.UniqueConstraintName(table, "email"))
	assert.Equal(t, schema.RoleColumnUnique, role)
	assert.Equal(t, "email", col)

	role, col, _ = schema.ClassifyConstraint(table, schema.ColumnCheckConstraintName(table, "age"))
	assert.Equal(t, schema.RoleColumnCheck, role)
	assert.Equal(t, "age", col)

	role, _, n := schema.ClassifyConstraint(table, schema.TableCheckConstraintName(table, 3))
	assert.Equal(t, schema.RoleTableCheck, role)
	assert.Equal(t, 3, n)
}

func TestNormalizeExpr(t *testing.T) {
	assert.Equal(t, "age >= 0", schema.NormalizeExpr("age   >=\n  0"))
	assert.Equal(t, "0", schema.NormalizeExpr("  0  "))
	assert.Empty(t, schema.NormalizeExpr("   "))
}
