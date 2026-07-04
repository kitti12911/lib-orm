package patchfield

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSourceEmitsSetClauses(t *testing.T) {
	spec := generateSpec{
		packageName:   "part",
		functionName:  "patchFields",
		paramType:     "PatchParams",
		dataType:      "patchData",
		pathsSelector: "params.Fields",
		buckets:       []bucket{{prefix: "", mapField: "partFields"}},
		setClauses: &setClausesSpec{
			funcName:  "partPatchSetClauses",
			bitFunc:   "database.Bit",
			bitImport: "example.com/svc/internal/database",
			cols: []setCol{
				{column: "code"},
				{column: "is_active", isBool: true},
			},
		},
	}

	src := renderSource(spec, nil)
	if _, err := format.Source(src); err != nil {
		t.Fatalf("generated source does not gofmt: %v\n%s", err, src)
	}
	out := string(src)

	assert.Contains(t, out, `"fmt"`)
	assert.Contains(t, out, `"sort"`)
	assert.Contains(t, out, `"example.com/svc/internal/database"`)
	assert.Contains(t, out, "func partPatchSetClauses(fields map[string]any) (setClauses []string, args []any, err error) {")
	assert.Contains(t, out, `setClauses = append(setClauses, "[code] = ?")`)
	// bool column routes through the configured bit conversion.
	assert.Contains(t, out, "if b, ok := value.(bool); ok {")
	assert.Contains(t, out, "value = database.Bit(b)")
	assert.Contains(t, out, `return nil, nil, fmt.Errorf("orm: invalid patch field %q", field)`)
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
	buckets := []bucket{
		{prefix: "profile.address", mapField: "addressFields"},
		{prefix: "profile", mapField: "profileFields"},
		{prefix: "", mapField: "rootFields"},
	}

	got := collectFields(models, buckets, "PatchBody", "", "input.Payload", nil, map[string]bool{})

	assert.Equal(t, []field{
		{
			path:     "email",
			key:      "email",
			selector: "input.Payload.Email",
			mapField: "rootFields",
		},
		{
			path:     "profile.first_name",
			key:      "first_name",
			selector: "input.Payload.Profile.FirstName",
			guards:   []string{"input.Payload.Profile"},
			mapField: "profileFields",
		},
		{
			path:     "profile.address.city",
			key:      "city",
			selector: "input.Payload.Profile.Address.City",
			guards:   []string{"input.Payload.Profile", "input.Payload.Profile.Address"},
			mapField: "addressFields",
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
		{prefix: "profile", mapField: "profileFields"},
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
	buckets := []bucket{
		{prefix: "profile", mapField: "profileFields"},
	}

	got, ok := bucketFor(buckets, "profile.first_name")
	require.True(t, ok)
	assert.Equal(t, bucket{prefix: "profile", mapField: "profileFields"}, got)

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

func TestGenerateEmitsCombinedNilGuard(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "params.go")
	writePatchFieldFile(t, src, `package user

type PatchBody struct {
	Profile *ProfileBody `+"`field:\"profile\"`"+`
}

type ProfileBody struct {
	Address *AddressBody `+"`field:\"address\"`"+`
}

type AddressBody struct {
	City *string `+"`field:\"city\"`"+`
}
`)
	out := filepath.Join(dir, "patch_generated.go")
	configPath := filepath.Join(dir, "patchfields.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
targets:
  - file: `+src+`
    root: PatchBody
    out: `+out+`
    package: user
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - path: profile.address
        map_field: addressFields
`), 0o600))

	require.NoError(t, Run([]string{"-config", configPath}))

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	got := string(data)
	// Combined guard short-circuits via `||` so a single ancestor nil check
	// avoids the deeper deref panic.
	assert.Contains(t, got, "params.Payload.Profile == nil || params.Payload.Profile.Address == nil")
	// Old chained-if pattern must not appear.
	assert.NotContains(t, got, "if params.Payload.Profile == nil {\n\t\t\t\tdata.addressFields")
}

func TestRunFromYAMLConfig(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "params.go")
	writePatchFieldFile(t, src, `package user

type PatchBody struct {
	Email string `+"`field:\"email\"`"+`
	Profile *ProfileBody `+"`field:\"profile\"`"+`
}

type ProfileBody struct {
	FirstName *string `+"`field:\"first_name\"`"+`
}
`)
	outA := filepath.Join(dir, "patch_a.go")
	outB := filepath.Join(dir, "patch_b.go")
	configPath := filepath.Join(dir, "patchfields.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
function: applyPatch
targets:
  - file: `+src+`
    root: PatchBody
    out: `+outA+`
    package: user
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - path: ""
        map_field: rootFields
      - path: profile
        map_field: profileFields
  - file: `+src+`
    root: PatchBody
    out: `+outB+`
    package: user
    function: applyPatchB
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - map_field: rootFields              # omitting "path" is equivalent to ""
`), 0o600))

	require.NoError(t, Run([]string{"-config", configPath}))

	dataA, err := os.ReadFile(outA)
	require.NoError(t, err)
	gotA := string(dataA)
	assert.Contains(t, gotA, "func applyPatch(params PatchParams) patchData")
	assert.Contains(t, gotA, `case "email":`)
	assert.Contains(t, gotA, `case "profile.first_name":`)
	assert.Contains(t, gotA, "data.profileFields[\"first_name\"] = params.Payload.Profile.FirstName")
	// No fieldmap import is emitted; the bucket map write is direct.
	assert.NotContains(t, gotA, "import fieldmap")

	dataB, err := os.ReadFile(outB)
	require.NoError(t, err)
	gotB := string(dataB)
	// Target B overrides the function name and inherits other defaults.
	assert.Contains(t, gotB, "func applyPatchB(params PatchParams) patchData")
	assert.Contains(t, gotB, `case "email":`)
}

func TestRunFromYAMLConfigWithCopyRules(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "params.go")
	writePatchFieldFile(t, src, `package user

type PatchBody struct {
	Profile *ProfileBody `+"`field:\"profile\"`"+`
}

type ProfileBody struct {
	Address *AddressBody `+"`field:\"address\"`"+`
}

type AddressBody struct {
	City *string `+"`field:\"city\"`"+`
}
`)
	out := filepath.Join(dir, "patch.go")
	configPath := filepath.Join(dir, "patchfields.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
targets:
  - file: `+src+`
    root: PatchBody
    out: `+out+`
    package: user
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - path: profile.address
        map_field: addressFields
    copies:
      - source: params.Payload.Profile
        target: data.profile
      - source: params.Payload.Profile.Address
        target: data.address
        guards:
          - params.Payload.Profile
`), 0o600))

	require.NoError(t, Run([]string{"-config", configPath}))

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	got := string(data)
	assert.Contains(t, got, "if params.Payload.Profile != nil {")
	assert.Contains(t, got, "data.profile = *params.Payload.Profile")
	assert.Contains(t, got, "if params.Payload.Profile != nil && params.Payload.Profile.Address != nil {")
	assert.Contains(t, got, "data.address = *params.Payload.Profile.Address")
}

func TestRunFromYAMLConfigErrors(t *testing.T) {
	dir := t.TempDir()
	writeYAML := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "patchfields.yaml")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		return path
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "missing file",
			args: []string{"-config", filepath.Join(dir, "nope.yaml")},
			want: "read config",
		},
		{
			name: "malformed yaml",
			args: []string{"-config", writeYAML(t, ":\n  - bad\n")},
			want: "parse config",
		},
		{
			name: "empty targets",
			args: []string{"-config", writeYAML(t, "targets: []\n")},
			want: "targets must not be empty",
		},
		{
			name: "target missing required fields",
			args: []string{"-config", writeYAML(t, `
targets:
  - file: x.go
    root: PatchBody
`)},
			want: "file, root, out, package, root_selector",
		},
		{
			name: "bucket missing fields",
			args: []string{"-config", writeYAML(t, `
targets:
  - file: x.go
    root: PatchBody
    out: y.go
    package: u
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - path: ""
`)},
			want: "map_field is required",
		},
		{
			name: "copy rule missing target",
			args: []string{"-config", writeYAML(t, `
targets:
  - file: x.go
    root: PatchBody
    out: y.go
    package: u
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - path: ""
        map_field: rootFields
    copies:
      - source: params.Payload.Profile
`)},
			want: "source and target are required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Run(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestRunRequiresConfigFlag(t *testing.T) {
	err := Run(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-config is required")
}

func TestRunFlagParseError(t *testing.T) {
	err := Run([]string{"-not-a-flag"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "flag provided but not defined")
}

func writeYAMLConfig(t *testing.T, srcPath, outPath string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "patchfields.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
targets:
  - file: `+srcPath+`
    root: PatchBody
    out: `+outPath+`
    package: u
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - map_field: rootFields
`), 0o600))
	return configPath
}

func TestRunParseModelError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "missing.go")
	configPath := writeYAMLConfig(t, src, filepath.Join(t.TempDir(), "out.go"))

	err := Run([]string{"-config", configPath})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse model file")
}

func TestRunFormatSourceError(t *testing.T) {
	src := filepath.Join(t.TempDir(), "p.go")
	writePatchFieldFile(t, src, `package u
type PatchBody struct { Email string `+"`field:\"email\"`"+` }
`)
	out := filepath.Join(t.TempDir(), "out.go")
	configPath := filepath.Join(t.TempDir(), "patchfields.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`
targets:
  - file: `+src+`
    root: PatchBody
    out: `+out+`
    package: "1invalid"
    root_selector: params.Payload
    paths_selector: params.Fields
    buckets:
      - map_field: rootFields
`), 0o600))

	err := Run([]string{"-config", configPath})
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

	out := filepath.Join(blocker, "sub", "out.go")
	configPath := writeYAMLConfig(t, src, out)

	err := Run([]string{"-config", configPath})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mkdir")
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
