package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStringList(t *testing.T) {
	var list stringList

	require.NoError(t, list.Set("one"))
	require.NoError(t, list.Set("two"))
	require.Error(t, list.Set(""))
	assert.Equal(t, "one,two", list.String())
}

func TestParseBuckets(t *testing.T) {
	got, err := parseBuckets([]string{
		"root:rootFields:fieldmap.IsRootField",
		"profile:profileFields:fieldmap.IsProfileField",
		"profile.address:addressFields:fieldmap.IsAddressField",
	})

	require.NoError(t, err)
	assert.Equal(t, []bucket{
		{prefix: "profile.address", mapField: "addressFields", validator: "fieldmap.IsAddressField"},
		{prefix: "profile", mapField: "profileFields", validator: "fieldmap.IsProfileField"},
		{prefix: "", mapField: "rootFields", validator: "fieldmap.IsRootField"},
	}, got)
}

func TestParseBucketsReturnsInvalidBucket(t *testing.T) {
	_, err := parseBuckets([]string{"bad"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid bucket")
}

func TestParseCopyRules(t *testing.T) {
	got, err := parseCopyRules([]string{
		"input.Payload.Profile:data.profile",
		"input.Payload.Profile.Address:data.address:input.Payload.Profile",
	})

	require.NoError(t, err)
	assert.Equal(t, []copyRule{
		{source: "input.Payload.Profile", target: "data.profile"},
		{source: "input.Payload.Profile.Address", target: "data.address", guards: []string{"input.Payload.Profile"}},
	}, got)
}

func TestParseCopyRulesReturnsInvalidRule(t *testing.T) {
	_, err := parseCopyRules([]string{"bad"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid copy")
}

func TestCollectFieldsUsesConfiguredSelectorsAndBuckets(t *testing.T) {
	file := filepath.Join(t.TempDir(), "params.go")
	writePatchFieldFile(t, file, `
package user

type PatchBody struct {
	Email string `+"`field:\"email\"`"+`
	Profile *ProfileBody `+"`field:\"profile\"`"+`
}

type ProfileBody struct {
	FirstName *string `+"`field:\"first_name\"`"+`
	Address *AddressBody `+"`field:\"address\"`"+`
}

type AddressBody struct {
	City *string `+"`field:\"city\"`"+`
}
`)

	models, err := parseModels(file)
	require.NoError(t, err)
	buckets, err := parseBuckets([]string{
		"root:rootFields:fieldmap.IsRootField",
		"profile:profileFields:fieldmap.IsProfileField",
		"profile.address:addressFields:fieldmap.IsAddressField",
	})
	require.NoError(t, err)

	got := collectFields(models, buckets, "PatchBody", "", "input.Payload", nil, map[string]bool{})

	assert.Equal(t, []field{
		{
			path:      "email",
			key:       "email",
			selector:  "input.Payload.Email",
			mapField:  "rootFields",
			validator: "fieldmap.IsRootField",
		},
		{
			path:      "profile.first_name",
			key:       "first_name",
			selector:  "input.Payload.Profile.FirstName",
			guards:    []string{"input.Payload.Profile"},
			mapField:  "profileFields",
			validator: "fieldmap.IsProfileField",
		},
		{
			path:      "profile.address.city",
			key:       "city",
			selector:  "input.Payload.Profile.Address.City",
			guards:    []string{"input.Payload.Profile", "input.Payload.Profile.Address"},
			mapField:  "addressFields",
			validator: "fieldmap.IsAddressField",
		},
	}, got)
}

func TestCollectFieldsSkipsMissingModelAndMissingBucket(t *testing.T) {
	models := map[string]model{
		"PatchBody": {
			name: "PatchBody",
			fields: []modelField{
				{name: "Email", tag: "email", typeName: "string"},
			},
		},
	}
	buckets := []bucket{
		{prefix: "profile", mapField: "profileFields", validator: "fieldmap.IsProfileField"},
	}

	assert.Nil(t, collectFields(models, buckets, "MissingBody", "", "input.Payload", nil, map[string]bool{}))
	assert.Empty(t, collectFields(models, buckets, "PatchBody", "", "input.Payload", nil, map[string]bool{}))
}

func TestParseModelsReturnsParseError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "bad.go")
	writePatchFieldFile(t, file, `package user
type Broken struct {`)

	_, err := parseModels(file)

	require.Error(t, err)
}

func TestParseModelsSkipsUnsupportedDeclarations(t *testing.T) {
	file := filepath.Join(t.TempDir(), "params.go")
	writePatchFieldFile(t, file, `
package user

func helper() {}

type Alias = string

type PatchBody struct {
	Email string `+"`field:\"email\"`"+`
}
`)

	got, err := parseModels(file)

	require.NoError(t, err)
	assert.Contains(t, got, "PatchBody")
	assert.NotContains(t, got, "Alias")
}

func TestParseModelSkipsUnsupportedFields(t *testing.T) {
	file := parseSource(t, `package user
type Embedded struct{}
type PatchBody struct {
	Embedded `+"`field:\"ignored_embedded\"`"+`
	IgnoredNoTag string
	IgnoredUnsupported []string `+"`field:\"ignored_unsupported\"`"+`
	Email string `+"`field:\"email\"`"+`
}`)
	spec := file.Decls[1].(*ast.GenDecl).Specs[0].(*ast.TypeSpec)

	got := parseModel("PatchBody", spec.Type.(*ast.StructType))

	assert.Equal(t, model{
		name: "PatchBody",
		fields: []modelField{
			{name: "Email", tag: "email", typeName: "string"},
		},
	}, got)
}

func TestTypeName(t *testing.T) {
	file := parseSource(t, `package user
type Profile struct{}
type PatchBody struct {
	A Profile
	B *Profile
	C []Profile
}`)
	fields := file.Decls[1].(*ast.GenDecl).Specs[0].(*ast.TypeSpec).Type.(*ast.StructType).Fields.List

	tests := []struct {
		index   int
		name    string
		pointer bool
	}{
		{index: 0, name: "Profile", pointer: false},
		{index: 1, name: "Profile", pointer: true},
		{index: 2, name: "", pointer: false},
	}

	for _, tt := range tests {
		gotName, gotPointer := typeName(fields[tt.index].Type)
		assert.Equal(t, tt.name, gotName)
		assert.Equal(t, tt.pointer, gotPointer)
	}
}

func TestBucketFor(t *testing.T) {
	buckets, err := parseBuckets([]string{
		"profile:profileFields:fieldmap.IsProfileField",
	})
	require.NoError(t, err)

	got, ok := bucketFor(buckets, "profile.first_name")

	require.True(t, ok)
	assert.Equal(t, bucket{prefix: "profile", mapField: "profileFields", validator: "fieldmap.IsProfileField"}, got)

	_, ok = bucketFor(buckets, "email")
	assert.False(t, ok)
}

func TestMapFields(t *testing.T) {
	got := mapFields([]bucket{
		{mapField: "profileFields"},
		{mapField: "rootFields"},
		{mapField: "profileFields"},
	})

	assert.Equal(t, []string{"profileFields", "rootFields"}, got)
}

func TestNilConditions(t *testing.T) {
	got := nilConditions([]string{"input.Profile", "input.Profile.Address"})

	assert.Equal(t, "input.Profile != nil && input.Profile.Address != nil", got)
}

func TestCollectFieldsCyclicTypeTerminates(t *testing.T) {
	models := map[string]model{
		"A": {
			name: "A",
			fields: []modelField{
				{name: "B", tag: "b", typeName: "B"},
			},
		},
		"B": {
			name: "B",
			fields: []modelField{
				{name: "A", tag: "a", typeName: "A"},
			},
		},
	}
	done := make(chan struct{})
	go func() {
		collectFields(models, nil, "A", "", "input", nil, map[string]bool{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("collectFields did not terminate on cyclic types")
	}
}

func TestRunGeneratesOutput(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "params.go")
	writePatchFieldFile(t, src, `package user

type PatchBody struct {
	Email string `+"`field:\"email\"`"+`
}

type PatchParams struct {
	Payload PatchBody
	Fields  []string
}
`)
	out := filepath.Join(t.TempDir(), "sub", "patch_generated.go")

	err := run([]string{
		"-file", src,
		"-root", "PatchBody",
		"-out", out,
		"-package", "user",
		"-fieldmap-import", "example.com/fieldmap",
		"-root-selector", "params.Payload",
		"-paths-selector", "params.Fields",
		"-bucket", "root:rootFields:fieldmap.IsRootField",
	})
	require.NoError(t, err)

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, "package user")
	assert.Contains(t, got, `case "email":`)
}

func TestRunErrors(t *testing.T) {
	srcDir := t.TempDir()
	validSrc := filepath.Join(srcDir, "p.go")
	writePatchFieldFile(t, validSrc, `package u
type PatchBody struct { Email string `+"`field:\"email\"`"+` }
`)
	validArgs := func(overrides ...string) []string {
		args := []string{
			"-file", validSrc,
			"-root", "PatchBody",
			"-out", filepath.Join(t.TempDir(), "out.go"),
			"-package", "u",
			"-fieldmap-import", "example.com/fieldmap",
			"-root-selector", "params.Payload",
			"-paths-selector", "params.Fields",
			"-bucket", "root:rootFields:fieldmap.IsRootField",
		}
		return append(args, overrides...)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing required flag",
			args: []string{"-file", validSrc},
			want: "are required",
		},
		{
			name: "parse model error",
			args: validArgs("-file", filepath.Join(t.TempDir(), "missing.go")),
			want: "parse model file",
		},
		{
			name: "bad bucket spec",
			args: []string{
				"-file", validSrc,
				"-root", "PatchBody",
				"-out", filepath.Join(t.TempDir(), "out.go"),
				"-package", "u",
				"-fieldmap-import", "example.com/fieldmap",
				"-root-selector", "params.Payload",
				"-paths-selector", "params.Fields",
				"-bucket", "malformed",
			},
			want: "invalid bucket",
		},
		{
			name: "bad copy spec",
			args: append(validArgs(), "-copy", "only-one"),
			want: "invalid copy",
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
	src := filepath.Join(t.TempDir(), "p.go")
	writePatchFieldFile(t, src, `package u
type PatchBody struct { Email string `+"`field:\"email\"`"+` }
`)
	err := run([]string{
		"-file", src,
		"-root", "PatchBody",
		"-out", filepath.Join(t.TempDir(), "out.go"),
		"-package", "1invalid",
		"-fieldmap-import", "example.com/fieldmap",
		"-root-selector", "params.Payload",
		"-paths-selector", "params.Fields",
		"-bucket", "root:rootFields:fieldmap.IsRootField",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "format generated source")
}

func TestRunMkdirError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "p.go")
	writePatchFieldFile(t, src, `package u
type PatchBody struct { Email string `+"`field:\"email\"`"+` }
`)
	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte{}, 0o600))

	err := run([]string{
		"-file", src,
		"-root", "PatchBody",
		"-out", filepath.Join(blocker, "sub", "out.go"),
		"-package", "u",
		"-fieldmap-import", "example.com/fieldmap",
		"-root-selector", "params.Payload",
		"-paths-selector", "params.Fields",
		"-bucket", "root:rootFields:fieldmap.IsRootField",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mkdir")
}

func TestMainExitsOnError(t *testing.T) {
	if os.Getenv("PATCHFIELDGEN_TEST_MAIN") == "1" {
		os.Args = []string{"patchfieldgen"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestMainExitsOnError")
	cmd.Env = append(os.Environ(), "PATCHFIELDGEN_TEST_MAIN=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, stderr.String(), "patchfieldgen:")
}

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.txt")
	require.NoError(t, writeFileAtomic(path, []byte("hello"), 0o644))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	require.NoError(t, writeFileAtomic(path, []byte("world"), 0o644))
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "world", string(data))
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	err := writeFileAtomic(filepath.Join(t.TempDir(), "nope", "out.txt"), []byte("x"), 0o644)
	require.Error(t, err)
}

func writePatchFieldFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func parseSource(t *testing.T, src string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "source.go", src, 0)
	require.NoError(t, err)
	return file
}
