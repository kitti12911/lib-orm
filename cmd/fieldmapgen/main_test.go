package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnake(t *testing.T) {
	tests := map[string]string{
		"UserID":    "user_id",
		"HTTPSPort": "https_port",
		"URLValue":  "url_value",
		"Line1":     "line1",
	}

	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, snake(input))
		})
	}
}

func TestIsChildRelation(t *testing.T) {
	tests := map[string]bool{
		"rel:has-one,join:id=user_id":        true,
		"rel:has-many,join:id=user_id":       true,
		"rel:belongs-to,join:user_id=id":     false,
		"rel:many-to-many,join:user_id=id":   false,
		"column_name,type:uuid,default:null": false,
	}

	for tag, want := range tests {
		t.Run(tag, func(t *testing.T) {
			assert.Equal(t, want, isChildRelation(tag))
		})
	}
}

func TestStringList(t *testing.T) {
	var list stringList
	require.NoError(t, list.Set("User"))
	require.Error(t, list.Set(""))
	assert.Equal(t, "User", list.String())
}

func TestParseModelDirAndVisit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "model.go"), `
package database

import "github.com/uptrace/bun"

type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`
	ID string `+"`bun:\"id,pk\"`"+`
	UserName string `+"`bun:\"username\"`"+`
	Profile *UserProfile `+"`bun:\"rel:has-one,join:id=user_id\"`"+`
	Owner *User `+"`bun:\"rel:belongs-to,join:owner_id=id\"`"+`
}

type UserProfile struct {
	bun.BaseModel `+"`bun:\"table:user_profiles,alias:up\"`"+`
	ID string `+"`bun:\"id,pk\"`"+`
	UserID string `+"`bun:\"user_id\"`"+`
}
`)
	writeFile(t, filepath.Join(dir, "model_test.go"), `package database`)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o755))

	models, err := parseModelDir(dir)
	require.NoError(t, err)
	require.Len(t, models, 2)
	assert.Equal(t, "users", models["User"].table)
	assert.Equal(t, "u", models["User"].alias)
	assert.Equal(t, "username", models["User"].columns["user_name"])

	maps := map[string]fieldMap{}
	visit(models, models["User"], rootNestedName, maps, map[string]bool{})
	assert.Contains(t, maps, rootNestedName)
	assert.Contains(t, maps, "profile")
	assert.NotContains(t, maps, "owner")
}

func TestVisitSkipsSeenAndMissingRelationModels(t *testing.T) {
	models := map[string]model{
		"User": {
			name:    "User",
			columns: map[string]string{"id": "id"},
			relations: []relation{
				{name: "Profile", model: "Profile", child: true},
				{name: "Missing", model: "Missing", child: true},
			},
		},
		"Profile": {
			name:    "Profile",
			columns: map[string]string{"user_id": "user_id"},
			relations: []relation{
				{name: "Address", model: "Address", child: true},
			},
		},
		"Address": {
			name:    "Address",
			columns: map[string]string{"city": "city"},
		},
	}

	maps := map[string]fieldMap{}
	seen := map[string]bool{}
	visit(models, models["User"], rootNestedName, maps, seen)
	visit(models, models["User"], rootNestedName, maps, seen)

	assert.Contains(t, maps, rootNestedName)
	assert.Contains(t, maps, "profile")
	assert.Contains(t, maps, "profile.address")
	assert.NotContains(t, maps, "missing")
	assert.Len(t, maps, 3)
	assert.Equal(t, "profile", maps["profile"].alias)
	assert.Equal(t, "profile__address", maps["profile.address"].alias)
}

func TestParseModelFileError(t *testing.T) {
	_, err := parseModelFile(filepath.Join(t.TempDir(), "missing.go"))
	require.Error(t, err)
}

func TestParseModelDirError(t *testing.T) {
	_, err := parseModelDir(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}

func TestParseModelDirReturnsParseFileError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bad.go"), `package database
type Broken struct {`)

	_, err := parseModelDir(dir)
	require.Error(t, err)
}

func TestParseModelFileSkipsUnsupportedDeclarations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.go")
	writeFile(t, path, `
package database

import "github.com/uptrace/bun"

func helper() {}

type Alias = string

type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`
	ID string `+"`bun:\"id,pk\"`"+`
}
`)

	models, err := parseModelFile(path)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Contains(t, models, "User")
	assert.NotContains(t, models, "Alias")
}

func TestParseModelRejectsStructWithoutTable(t *testing.T) {
	file := parseSource(t, `package p; type NoTable struct { ID string `+"`bun:\"id\"`"+` }`)
	spec := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec)
	_, ok := parseModel("NoTable", spec.Type.(*ast.StructType))
	assert.False(t, ok)
}

func TestParseModelSkipsUnsupportedFields(t *testing.T) {
	file := parseSource(t, `package p
import "github.com/uptrace/bun"
type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`
	IgnoredNoTag string
	Embedded `+"`bun:\"embed\"`"+`
	IgnoredEmptyColumn string `+"`bun:\",nullzero\"`"+`
	UserID string `+"`bun:\"user_id\"`"+`
}`)
	spec := file.Decls[1].(*ast.GenDecl).Specs[0].(*ast.TypeSpec)

	got, ok := parseModel("User", spec.Type.(*ast.StructType))

	require.True(t, ok)
	assert.Equal(t, "users", got.table)
	assert.Equal(t, map[string]string{"user_id": "user_id"}, got.columns)
}

func TestStructTagAndTagOptionValue(t *testing.T) {
	file := parseSource(t, `package p; type T struct { Name string `+"`bun:\"name,type:text\"`"+` }`)
	field := file.Decls[0].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List[0]
	assert.Equal(t, "name,type:text", structTag(field).Get("bun"))
	assert.Equal(t, "users", tagOptionValue("table:users,alias:u", "table"))
	assert.Empty(t, tagOptionValue("alias:u", "table"))
}

func TestModelTypeName(t *testing.T) {
	file := parseSource(t, `package p
type User struct{}
type T struct {
	A *User
	B []User
	C pkg.User
	D map[string]User
}`)
	fields := file.Decls[1].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List
	tests := map[string]string{
		"A": "User",
		"B": "User",
		"C": "User",
		"D": "",
	}
	for _, field := range fields {
		name := field.Names[0].Name
		assert.Equal(t, tests[name], modelTypeName(field.Type))
	}
}

func TestWriteMap(t *testing.T) {
	var buf bytes.Buffer
	writeMap(&buf, "Fields", map[string]string{"b": "bee", "a": "aye"})
	want := "var Fields = map[string]string{\n\t\"a\": \"aye\",\n\t\"b\": \"bee\",\n}\n\n"
	assert.Equal(t, want, buf.String())
}

func TestColumnsFor(t *testing.T) {
	maps := map[string]fieldMap{
		rootNestedName: {
			fields: map[string]string{"email": "email"},
			alias:  "u",
		},
		"profile": {
			fields: map[string]string{"first_name": "first_name"},
			alias:  "profile",
		},
	}

	got := columnsFor(maps)

	assert.Equal(t, map[string]string{
		"email":              "u.email",
		"profile.first_name": "profile.first_name",
	}, got)
}

func TestQualifyColumn(t *testing.T) {
	assert.Equal(t, "u.email", qualifyColumn("u", "email"))
	assert.Equal(t, "email", qualifyColumn("", "email"))
}

func TestQueryAlias(t *testing.T) {
	assert.Equal(t, "u", queryAlias(model{alias: "u"}, rootNestedName))
	assert.Equal(t, "profile", queryAlias(model{alias: "up"}, "profile"))
	assert.Equal(t, "profile__address", queryAlias(model{alias: "ua"}, "profile.address"))
}

func TestStructTagWithoutTag(t *testing.T) {
	tag := structTag(&ast.Field{})
	assert.Equal(t, reflect.StructTag(""), tag)
}

func TestRunGeneratesOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "model.go"), `
package database

import "github.com/uptrace/bun"

type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`
	ID string `+"`bun:\"id,pk\"`"+`
	Email string `+"`bun:\"email\"`"+`
}
`)
	out := filepath.Join(t.TempDir(), "out", "fieldmap_generated.go")

	err := run([]string{
		"-model-dir", dir,
		"-out", out,
		"-package", "database",
		"-root", "User",
	})
	require.NoError(t, err)

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, "package database")
	assert.Contains(t, got, "var UserRootFields = map[string]string{")
	assert.Contains(t, got, "var UserColumns = map[string]string{")
	assert.Contains(t, got, `"email": "email"`)
	// Validator functions and the RootNestedName const are no longer emitted.
	assert.NotContains(t, got, "func IsUserRootField")
	assert.NotContains(t, got, "RootNestedName")
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing root",
			args: []string{"-model-dir", t.TempDir(), "-out", "/tmp/x.go"},
			want: "at least one -root is required",
		},
		{
			name: "model dir missing",
			args: []string{"-model-dir", filepath.Join(t.TempDir(), "nope"), "-out", "/tmp/x.go", "-root", "User"},
			want: "read model directory",
		},
		{
			name: "unknown root",
			args: []string{"-model-dir", t.TempDir(), "-out", "/tmp/x.go", "-root", "Ghost"},
			want: "Ghost model not found",
		},
		{
			name: "flag parse error",
			args: []string{"-not-a-flag"},
			want: "flag provided but not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestRunFormatSourceError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "model.go"), `
package database

import "github.com/uptrace/bun"

type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`
	ID string `+"`bun:\"id,pk\"`"+`
}
`)
	err := run([]string{
		"-model-dir", dir,
		"-out", filepath.Join(t.TempDir(), "out.go"),
		"-package", "1invalid",
		"-root", "User",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "format generated source")
}

func TestRunMkdirError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "model.go"), `
package database

import "github.com/uptrace/bun"

type User struct {
	bun.BaseModel `+"`bun:\"table:users,alias:u\"`"+`
	ID string `+"`bun:\"id,pk\"`"+`
}
`)
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte{}, 0o600))

	err := run([]string{
		"-model-dir", dir,
		"-out", filepath.Join(blocker, "sub", "out.go"),
		"-package", "database",
		"-root", "User",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mkdir")
}

func TestMainExitsOnError(t *testing.T) {
	if os.Getenv("FIELDMAPGEN_TEST_MAIN") == "1" {
		os.Args = []string{"fieldmapgen"}
		main()
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestMainExitsOnError") //nolint:gosec // os.Args[0] is the test binary path
	cmd.Env = append(os.Environ(), "FIELDMAPGEN_TEST_MAIN=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, stderr.String(), "fieldmapgen:")
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "out.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, writeFileAtomic(path, []byte("hello")))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	require.NoError(t, writeFileAtomic(path, []byte("world")))
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "world", string(data))
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	err := writeFileAtomic(filepath.Join(t.TempDir(), "nope", "out.txt"), []byte("x"))
	require.Error(t, err)
}

func parseSource(t *testing.T, src string) *ast.File {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "test.go", src, parser.ParseComments)
	require.NoError(t, err)
	return file
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(strings.TrimSpace(contents)), 0o600))
}
