package filtergen

import (
	"go/ast"
	"go/parser"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLowerFirst(t *testing.T) {
	assert.Equal(t, "", lowerFirst(""))
	assert.Equal(t, "foo", lowerFirst("Foo"))
	assert.Equal(t, "f", lowerFirst("F"))
	assert.Equal(t, "fOO", lowerFirst("FOO"))
}

func TestDirectivePresent(t *testing.T) {
	group := &ast.CommentGroup{List: []*ast.Comment{
		{Text: "// a comment"},
		{Text: "//mapgen:ignore"},
	}}
	assert.True(t, directivePresent(group, "ignore"))
	assert.False(t, directivePresent(group, "filter"))
}

func TestFilterCol(t *testing.T) {
	cg := func(text string) *ast.CommentGroup {
		return &ast.CommentGroup{List: []*ast.Comment{{Text: text}}}
	}

	col, ok := filterCol(cg("//mapgen:filter col=email"))
	assert.True(t, ok)
	assert.Equal(t, "email", col)

	_, ok = filterCol(cg("//mapgen:filter")) // no col= token
	assert.False(t, ok)

	_, ok = filterCol(cg("//mapgen:filter col=")) // empty col value
	assert.False(t, ok)

	_, ok = filterCol(cg("// not a directive")) // unrelated comment
	assert.False(t, ok)
}

// fixture writes a minimal repo (go.mod + models + feature packages) to a temp
// dir and returns its path.
func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	write := func(rel, content string) {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}

	write("go.mod", `module demo

go 1.26.4

require (
	github.com/kitti12911/lib-orm/v4 v4.0.0
	github.com/kitti12911/lib-util/v3 v3.17.0
)
`)

	write("internal/database/model.go", `package database

import "github.com/uptrace/bun"

type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`

	ID      string       `+"`bun:\"id\"`"+`
	Email   string       `+"`bun:\"email\"`"+`
	Profile *UserProfile `+"`bun:\"rel:has-one,join:id=user_id\"`"+`
}

type UserProfile struct {
	bun.BaseModel `+"`bun:\"table:user_profiles,alias:up\"`"+`

	ID        string `+"`bun:\"id\"`"+`
	UserID    string `+"`bun:\"user_id\"`"+`
	FirstName string `+"`bun:\"first_name\"`"+`
	User      *User  `+"`bun:\"rel:belongs-to,join:user_id=id\"`"+`
}
`)

	// user feature: has a custom filter directive
	write("internal/feature/user/query.go", `package user

import orm "github.com/kitti12911/lib-orm/v4"

//mapgen:filter col=full_name
func filterFullName(f orm.Filter) (string, []any, error) {
	return "concat(u.first) ILIKE ?", []any{"%x%"}, nil
}
`)

	// worker feature: no matching root model → skipped
	write("internal/feature/worker/worker.go", "package worker\n")

	return dir
}

func TestRunGeneratesUserFilter(t *testing.T) {
	dir := fixture(t)

	require.NoError(t, Run([]string{"-C", dir}))

	got, err := os.ReadFile(filepath.Join(dir, "internal", "feature", "user", "filter_generated.go"))
	require.NoError(t, err)
	src := string(got)

	assert.Contains(t, src, "package user")
	assert.Contains(t, src, `"github.com/kitti12911/lib-util/v3/apperror"`)
	assert.Contains(t, src, `fieldmap "demo/gen/database"`)
	assert.Contains(t, src, `orm "github.com/kitti12911/lib-orm/v4"`)
	assert.Contains(t, src, "var userCustomFilters = map[string]orm.FilterExpr{")
	assert.Contains(t, src, `"full_name": filterFullName,`)
	assert.Contains(t, src, "orm.ApplyFilter(query, filter, fieldmap.UserColumns, userCustomFilters)")
	assert.Contains(t, src, "orm.ApplyOrderBy(query, orderBy, fieldmap.UserColumns)")
	assert.Contains(t, src, `apperror.InvalidInput("invalid filter", err)`)
	assert.Contains(t, src, `apperror.InvalidInput("invalid order by", err)`)
}

func TestRunSkipsFeatureWithoutRootModel(t *testing.T) {
	dir := fixture(t)

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(dir, "internal", "feature", "worker", "filter_generated.go"))
	assert.True(t, os.IsNotExist(err), "worker has no root model, must be skipped")
}

func TestRunHonorsIgnoreDirective(t *testing.T) {
	dir := fixture(t)
	// Prepend a package-level ignore directive to the user feature.
	path := filepath.Join(dir, "internal", "feature", "user", "doc.go")
	require.NoError(t, os.WriteFile(path, []byte("//mapgen:ignore\npackage user\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(dir, "internal", "feature", "user", "filter_generated.go"))
	assert.True(t, os.IsNotExist(err), "ignored feature must be skipped")
}

func TestRunNoCustomFilters(t *testing.T) {
	dir := fixture(t)
	// Replace the user query.go with one that has no directive.
	path := filepath.Join(dir, "internal", "feature", "user", "query.go")
	require.NoError(t, os.WriteFile(path, []byte("package user\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))

	got, err := os.ReadFile(filepath.Join(dir, "internal", "feature", "user", "filter_generated.go"))
	require.NoError(t, err)
	assert.Contains(t, string(got), "var userCustomFilters = map[string]orm.FilterExpr{}")
}

func TestRunIdempotent(t *testing.T) {
	dir := fixture(t)

	require.NoError(t, Run([]string{"-C", dir}))
	first, err := os.ReadFile(filepath.Join(dir, "internal", "feature", "user", "filter_generated.go"))
	require.NoError(t, err)

	require.NoError(t, Run([]string{"-C", dir}))
	second, err := os.ReadFile(filepath.Join(dir, "internal", "feature", "user", "filter_generated.go"))
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
}

func TestRunErrors(t *testing.T) {
	t.Run("bad flag", func(t *testing.T) {
		assert.Error(t, Run([]string{"-nope"}))
	})

	t.Run("missing go.mod", func(t *testing.T) {
		assert.Error(t, Run([]string{"-C", t.TempDir()}))
	})

	t.Run("no module line", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("go 1.26.4\n"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})

	t.Run("no lib-util require", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})

	t.Run("missing model dir", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module demo\nrequire github.com/kitti12911/lib-util/v3 v3.17.0\n"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})

	t.Run("missing feature dir", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
			[]byte("module demo\nrequire github.com/kitti12911/lib-util/v3 v3.17.0\n"), 0o600))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "database"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "database", "m.go"), []byte("package database\n"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})
}

func TestRelationModel(t *testing.T) {
	cases := map[string]string{
		"Foo":            "Foo", // ident
		"*Foo":           "Foo", // pointer -> ident
		"[]Foo":          "Foo", // slice -> ident
		"[]*Foo":         "Foo", // slice -> pointer -> ident
		"pkg.Foo":        "Foo", // selector
		"*pkg.Foo":       "Foo", // pointer -> selector
		"map[string]Foo": "",    // unsupported type -> empty
		"42":             "",    // literal -> empty
	}
	for src, want := range cases {
		expr, err := parser.ParseExpr(src)
		require.NoError(t, err)
		assert.Equal(t, want, relationModel(expr), "expr %q", src)
	}
}
