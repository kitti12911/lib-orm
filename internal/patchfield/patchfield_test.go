package patchfield

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const featureSource = `package user

type CreateParams struct {
	Email    string               ` + "`field:\"email\"`" + `
	Username string               ` + "`field:\"username\"`" + `
	Profile  *CreateProfileParams ` + "`field:\"profile\"`" + `
}

type CreateProfileParams struct {
	FirstName *string              ` + "`field:\"first_name\"`" + `
	Address   *CreateAddressParams ` + "`field:\"address\"`" + `
}

type CreateAddressParams struct {
	City *string ` + "`field:\"city\"`" + `
}

type PatchParams struct {
	ID     string
	User   CreateParams
	Fields []string
}
`

func writeFeature(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	fdir := filepath.Join(dir, "internal", "feature", "user")
	require.NoError(t, os.MkdirAll(fdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fdir, "user.go"), []byte(source), 0o600))
	return dir
}

func TestRunGeneratesPatch(t *testing.T) {
	dir := writeFeature(t, featureSource)

	require.NoError(t, Run([]string{"-C", dir}))

	got, err := os.ReadFile(filepath.Join(dir, "internal", "feature", "user", "patch_generated.go"))
	require.NoError(t, err)
	src := string(got)

	// patchData struct: one map per bucket + one field per nested pointer copy.
	assert.Contains(t, src, "type patchData struct {")
	assert.Contains(t, src, "userFields    map[string]any")
	assert.Contains(t, src, "profileFields map[string]any")
	assert.Contains(t, src, "addressFields map[string]any")
	assert.Contains(t, src, "profile       CreateProfileParams")
	assert.Contains(t, src, "address       CreateAddressParams")

	// dispatcher
	assert.Contains(t, src, "func patchFields(params PatchParams) patchData {")
	assert.Contains(t, src, "if params.User.Profile != nil {\n\t\tdata.profile = *params.User.Profile")
	assert.Contains(t, src, "if params.User.Profile != nil && params.User.Profile.Address != nil {")
	assert.Contains(t, src, `data.userFields["email"] = params.User.Email`)
	assert.Contains(t, src, `case "profile.first_name":`)
	assert.Contains(t, src, `data.profileFields["first_name"] = params.User.Profile.FirstName`)
	assert.Contains(t, src, `case "profile.address.city":`)
	assert.Contains(t, src, `data.addressFields["city"] = params.User.Profile.Address.City`)
}

func TestRunSkipsFeatureWithoutPatchParams(t *testing.T) {
	dir := t.TempDir()
	fdir := filepath.Join(dir, "internal", "feature", "worker")
	require.NoError(t, os.MkdirAll(fdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(fdir, "worker.go"), []byte("package worker\n"), 0o600))

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(fdir, "patch_generated.go"))
	assert.True(t, os.IsNotExist(err))
}

func TestRunHonorsIgnore(t *testing.T) {
	dir := writeFeature(t, "//mapgen:ignore\n"+featureSource)

	require.NoError(t, Run([]string{"-C", dir}))

	_, err := os.Stat(filepath.Join(dir, "internal", "feature", "user", "patch_generated.go"))
	assert.True(t, os.IsNotExist(err))
}

func TestRunIdempotent(t *testing.T) {
	dir := writeFeature(t, featureSource)
	path := filepath.Join(dir, "internal", "feature", "user", "patch_generated.go")

	require.NoError(t, Run([]string{"-C", dir}))
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, Run([]string{"-C", dir}))
	second, err := os.ReadFile(path)
	require.NoError(t, err)

	assert.Equal(t, string(first), string(second))
}

func TestRunErrors(t *testing.T) {
	t.Run("flag parse error", func(t *testing.T) {
		assert.Error(t, Run([]string{"-nope"}))
	})

	t.Run("missing feature dir", func(t *testing.T) {
		assert.Error(t, Run([]string{"-C", t.TempDir()}))
	})

	t.Run("parse error in feature", func(t *testing.T) {
		dir := t.TempDir()
		fdir := filepath.Join(dir, "internal", "feature", "user")
		require.NoError(t, os.MkdirAll(fdir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(fdir, "bad.go"), []byte("package user\ntype Broken struct {"), 0o600))
		assert.Error(t, Run([]string{"-C", dir}))
	})
}

func TestDetectPatchParams(t *testing.T) {
	parse := func(src string) *ast.StructType {
		f, err := parser.ParseFile(token.NewFileSet(), "t.go", "package p\ntype S struct {\n"+src+"\n}", 0)
		require.NoError(t, err)
		return f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType)
	}

	got := detectPatchParams(parse("ID string\nUser CreateParams\nFields []string"))
	require.NotNil(t, got)
	assert.Equal(t, "User", got.payloadField)
	assert.Equal(t, "CreateParams", got.payloadType)
	assert.Equal(t, "Fields", got.pathsField)

	assert.Nil(t, detectPatchParams(parse("A Foo\nB Bar\nFields []string"))) // ambiguous payload
	assert.Nil(t, detectPatchParams(parse("User CreateParams")))             // no paths field
	assert.Nil(t, detectPatchParams(parse("Fields []string")))               // no payload
	assert.Nil(t, detectPatchParams(parse("Embedded\nFields []string")))     // embedded skipped
}

func TestLastSegment(t *testing.T) {
	assert.Equal(t, "address", lastSegment("profile.address"))
	assert.Equal(t, "profile", lastSegment("profile"))
	assert.Equal(t, "", lastSegment(""))
}

func TestBucketFor(t *testing.T) {
	buckets := []bucket{
		{prefix: "profile.address", mapField: "addressFields"},
		{prefix: "profile", mapField: "profileFields"},
		{prefix: "", mapField: "userFields"},
	}

	tests := map[string]string{
		"email":                "userFields",
		"profile.first_name":   "profileFields",
		"profile.address.city": "addressFields",
	}
	for path, want := range tests {
		b, ok := bucketFor(buckets, path)
		require.True(t, ok, path)
		assert.Equal(t, want, b.mapField, path)
	}

	_, ok := bucketFor(nil, "x")
	assert.False(t, ok)
}

func TestMapFields(t *testing.T) {
	got := mapFields([]bucket{
		{mapField: "userFields"},
		{mapField: "profileFields"},
		{mapField: "userFields"}, // duplicate collapses
	})
	assert.Equal(t, []string{"profileFields", "userFields"}, got)
}

func TestNilConditions(t *testing.T) {
	assert.Equal(t, "a != nil && b != nil", nilConditions([]string{"a", "b"}))
}

func TestCombinedNilGuard(t *testing.T) {
	assert.Equal(t, "", combinedNilGuard(nil))
	assert.Equal(t, "a == nil || a.b == nil", combinedNilGuard([]string{"a", "a.b"}))
}

func TestTypeName(t *testing.T) {
	parse := func(expr string) ast.Expr {
		f, err := parser.ParseFile(token.NewFileSet(), "t.go", "package p\nvar x "+expr, 0)
		require.NoError(t, err)
		return f.Decls[0].(*ast.GenDecl).Specs[0].(*ast.ValueSpec).Type
	}
	name, ptr := typeName(parse("*Foo"))
	assert.Equal(t, "Foo", name)
	assert.True(t, ptr)
	name, ptr = typeName(parse("Foo"))
	assert.Equal(t, "Foo", name)
	assert.False(t, ptr)
	name, _ = typeName(parse("[]Foo"))
	assert.Equal(t, "", name)
}

func TestGenerateMkdirError(t *testing.T) {
	dir := writeFeature(t, featureSource)
	spec := generateSpec{
		file:          filepath.Join(dir, "internal", "feature", "user"),
		root:          "CreateParams",
		out:           filepath.Join(dir, "blocker", "sub", outFileName),
		packageName:   "user",
		rootSelector:  "params.User",
		pathsSelector: "params.Fields",
		buckets:       []bucket{{prefix: "", mapField: "userFields"}},
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blocker"), []byte{}, 0o600))
	assert.Error(t, generate(spec))
}
