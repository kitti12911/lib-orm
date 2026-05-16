// Command patchfieldgen generates per-field patch dispatchers for partial-update
// handlers. Given a Go struct describing the patch payload and a set of buckets
// that group fields by their destination map, it emits a function that walks
// the requested field paths and copies each value into the right bucket map.
//
// Configuration is supplied via a YAML file:
//
//	patchfieldgen -config patchfields.yaml
//
// A single config file can describe many targets, each producing one output
// file. Top-level keys (function, param_type, data_type) act as defaults
// inherited by every target unless that target overrides them.
//
// The generated code restricts writes to fields tagged with `field:"..."` in
// the source struct, so it carries no extra runtime allowlist check. If a path
// arrives that has no matching `case`, the switch silently falls through; if a
// path matches but the target column has been removed from the database, the
// downstream SQL UPDATE fails loudly rather than silently dropping the field.
//
// Earlier versions of this tool accepted everything via individual command-line
// flags and emitted a per-field validator call. Both are gone; pinning to a
// previous generator release keeps old invocations working, but upgrading
// requires switching to a YAML config and regenerating the output file.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// field is the fully-resolved info needed to emit one `case` clause: which
// path the caller will request, which bucket map receives the value, and the
// dotted Go selector used to read the value from params.
type field struct {
	path     string
	key      string
	selector string
	guards   []string
	mapField string
}

// model captures one struct type from the parsed source file along with the
// subset of its fields that participate in patches (tagged with `field:"..."`).
type model struct {
	name   string
	fields []modelField
}

type modelField struct {
	name      string
	tag       string
	typeName  string
	isPointer bool
}

// bucket maps a path prefix (e.g. "profile.address") to the destination map
// field name that owns every patchable leaf under that prefix.
type bucket struct {
	prefix   string
	mapField string
}

// copyRule expresses a guarded pointer copy emitted before the path switch.
// It produces `if guards... && source != nil { target = *source }`.
type copyRule struct {
	source string
	target string
	guards []string
}

// generateSpec is the fully-resolved input to one generation run. Every YAML
// target translates into one generateSpec before invoking generate().
type generateSpec struct {
	file          string
	root          string
	out           string
	packageName   string
	functionName  string
	paramType     string
	dataType      string
	rootSelector  string
	pathsSelector string
	buckets       []bucket
	copyRules     []copyRule
}

// yamlConfig mirrors the on-disk YAML schema. Top-level keys provide defaults
// inherited by every target unless that target overrides them. Targets is
// required and must be non-empty.
type yamlConfig struct {
	ParamType string       `yaml:"param_type"`
	DataType  string       `yaml:"data_type"`
	Function  string       `yaml:"function"`
	Targets   []yamlTarget `yaml:"targets"`
}

type yamlTarget struct {
	File          string       `yaml:"file"`
	Root          string       `yaml:"root"`
	Out           string       `yaml:"out"`
	Package       string       `yaml:"package"`
	Function      string       `yaml:"function"`
	ParamType     string       `yaml:"param_type"`
	DataType      string       `yaml:"data_type"`
	RootSelector  string       `yaml:"root_selector"`
	PathsSelector string       `yaml:"paths_selector"`
	Buckets       []yamlBucket `yaml:"buckets"`
	Copies        []yamlCopy   `yaml:"copies"`
}

type yamlBucket struct {
	// Path is the path prefix this bucket captures. Empty string (or just
	// omitting the key) means the top-level fields with no prefix.
	Path     string `yaml:"path"`
	MapField string `yaml:"map_field"`
}

type yamlCopy struct {
	Source string   `yaml:"source"`
	Target string   `yaml:"target"`
	Guards []string `yaml:"guards"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "patchfieldgen: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable entrypoint. It parses the -config flag and runs the
// generation pipeline once per target listed in the YAML config.
func run(args []string) error {
	fs := flag.NewFlagSet("patchfieldgen", flag.ContinueOnError)
	configPath := fs.String("config", "", "Path to a YAML config file describing one or more generation targets")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	if *configPath == "" {
		return errors.New("-config is required")
	}

	specs, err := specsFromConfig(*configPath)
	if err != nil {
		return err
	}

	for _, spec := range specs {
		if err := generate(spec); err != nil {
			return fmt.Errorf("generate %s: %w", spec.out, err)
		}
	}
	return nil
}

// specsFromConfig loads the YAML config and produces one spec per target. Each
// target inherits unset string fields from the top-level config defaults so the
// caller can DRY shared values like fieldmap_import.
func specsFromConfig(path string) ([]generateSpec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // generator is run by trusted operators on trusted config files
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg yamlConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	if len(cfg.Targets) == 0 {
		return nil, fmt.Errorf("config %q: targets must not be empty", path)
	}

	specs := make([]generateSpec, 0, len(cfg.Targets))
	for i, t := range cfg.Targets {
		spec, err := specFromTarget(cfg, t)
		if err != nil {
			return nil, fmt.Errorf("config %q target[%d]: %w", path, i, err)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// specFromTarget resolves a yamlTarget into a generateSpec, inheriting defaults
// from the surrounding yamlConfig and applying built-in defaults for the
// optional string fields.
func specFromTarget(cfg yamlConfig, t yamlTarget) (generateSpec, error) {
	first := func(values ...string) string {
		for _, v := range values {
			if v != "" {
				return v
			}
		}
		return ""
	}

	if t.File == "" || t.Root == "" || t.Out == "" || t.Package == "" ||
		t.RootSelector == "" || t.PathsSelector == "" || len(t.Buckets) == 0 {
		return generateSpec{}, errors.New("file, root, out, package, root_selector, paths_selector, and buckets are required")
	}

	buckets := make([]bucket, 0, len(t.Buckets))
	for _, b := range t.Buckets {
		if b.MapField == "" {
			return generateSpec{}, fmt.Errorf("bucket %q: map_field is required", b.Path)
		}
		buckets = append(buckets, bucket{
			prefix:   b.Path,
			mapField: b.MapField,
		})
	}
	// Longest prefix wins during bucket lookup so nested paths route correctly.
	sort.SliceStable(buckets, func(i, j int) bool {
		return len(buckets[i].prefix) > len(buckets[j].prefix)
	})

	copyRules := make([]copyRule, 0, len(t.Copies))
	for _, c := range t.Copies {
		if c.Source == "" || c.Target == "" {
			return generateSpec{}, errors.New("copy rule: source and target are required")
		}
		copyRules = append(copyRules, copyRule{
			source: c.Source,
			target: c.Target,
			guards: append([]string(nil), c.Guards...),
		})
	}

	return generateSpec{
		file:          t.File,
		root:          t.Root,
		out:           t.Out,
		packageName:   t.Package,
		functionName:  first(t.Function, cfg.Function, "patchFields"),
		paramType:     first(t.ParamType, cfg.ParamType, "PatchParams"),
		dataType:      first(t.DataType, cfg.DataType, "patchData"),
		rootSelector:  t.RootSelector,
		pathsSelector: t.PathsSelector,
		buckets:       buckets,
		copyRules:     copyRules,
	}, nil
}

// generate runs the full pipeline for one spec: parse the source, walk the
// struct tree to collect every patchable field, emit Go source, gofmt it, and
// atomically write it to disk.
func generate(spec generateSpec) error {
	models, err := parseModels(spec.file)
	if err != nil {
		return err
	}

	fields := collectFields(models, spec.buckets, spec.root, "", spec.rootSelector, nil, map[string]bool{})
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].path < fields[j].path
	})

	src := renderSource(spec, fields)
	out, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("format generated source: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(spec.out), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := writeFileAtomic(spec.out, out); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// renderSource produces the un-gofmt'd Go source for one generated file. The
// emitted function declares its data buckets, executes any unconditional copy
// rules, then dispatches per requested path to write the value directly into
// its bucket map, falling back to nil whenever a guarded pointer ancestor is
// unset. Validation of which paths are writable is the caller's responsibility
// upstream — the generator already restricts emission to fields tagged in the
// source struct.
func renderSource(spec generateSpec, fields []field) []byte {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by patchfieldgen; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", spec.packageName)
	fmt.Fprintf(&buf, "func %s(params %s) %s {\n", spec.functionName, spec.paramType, spec.dataType)

	// Pre-allocate one map per bucket so the switch can index without checks.
	fmt.Fprintf(&buf, "\tdata := %s{\n", spec.dataType)
	for _, mapField := range mapFields(spec.buckets) {
		fmt.Fprintf(&buf, "\t\t%s: make(map[string]any),\n", mapField)
	}
	buf.WriteString("\t}\n\n")

	// Copy rules run unconditionally up front and produce
	// `if guards != nil && source != nil { target = *source }`.
	for _, rule := range spec.copyRules {
		conditions := append(append([]string{}, rule.guards...), rule.source)
		fmt.Fprintf(&buf, "\tif %s {\n", nilConditions(conditions))
		fmt.Fprintf(&buf, "\t\t%s = *%s\n", rule.target, rule.source)
		buf.WriteString("\t}\n\n")
	}

	// Path dispatch. For each requested path, when the pointer chain leading
	// to the leaf is set we copy the value; otherwise we record a nil so the
	// downstream UPDATE clears the column.
	fmt.Fprintf(&buf, "\tfor _, path := range %s {\n", spec.pathsSelector)
	buf.WriteString("\t\tswitch path {\n")
	for _, f := range fields {
		fmt.Fprintf(&buf, "\t\tcase %q:\n", f.path)
		if guard := combinedNilGuard(f.guards); guard != "" {
			// One combined `||` short-circuits safely: an earlier nil check
			// prevents the later deref from panicking.
			fmt.Fprintf(&buf, "\t\t\tif %s {\n", guard)
			fmt.Fprintf(&buf, "\t\t\t\tdata.%s[%q] = nil\n", f.mapField, f.key)
			buf.WriteString("\t\t\t\tcontinue\n")
			buf.WriteString("\t\t\t}\n")
		}
		fmt.Fprintf(&buf, "\t\t\tdata.%s[%q] = %s\n", f.mapField, f.key, f.selector)
	}
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n\n")
	buf.WriteString("\treturn data\n")
	buf.WriteString("}\n")
	return buf.Bytes()
}

// combinedNilGuard joins a chain of pointer ancestors into a single boolean
// expression of the form `a == nil || a.b == nil || ...`. The Go `||` operator
// short-circuits, so each subsequent dereference is only evaluated when its
// ancestors are non-nil.
func combinedNilGuard(guards []string) string {
	if len(guards) == 0 {
		return ""
	}
	parts := make([]string, len(guards))
	for i, g := range guards {
		parts[i] = g + " == nil"
	}
	return strings.Join(parts, " || ")
}

// writeFileAtomic writes data to path by first writing to a temp file in the
// same directory and renaming on success, so a failed write never leaves a
// partial file at path. The output file mode is 0o644.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".patchfieldgen-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup; rename removes the file on success

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// parseModels parses path with go/parser and returns every top-level struct
// type indexed by name. Non-struct declarations are skipped.
func parseModels(path string) (map[string]model, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse model file: %w", err)
	}

	models := make(map[string]model)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}

		for _, spec := range gen.Specs {
			typeSpec := spec.(*ast.TypeSpec)
			strct, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			models[typeSpec.Name.Name] = parseModel(typeSpec.Name.Name, strct)
		}
	}

	return models, nil
}

// parseModel pulls the patch-relevant fields out of one struct. A field
// participates only when it has both a `field:"..."` struct tag and a type
// expression we can resolve to a name (plain identifier or pointer to one).
func parseModel(name string, strct *ast.StructType) model {
	m := model{name: name}
	for _, field := range strct.Fields.List {
		if len(field.Names) == 0 {
			continue
		}

		tag := ""
		if field.Tag != nil {
			tag = reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("field")
		}
		if tag == "" {
			continue
		}

		typeName, isPointer := typeName(field.Type)
		if typeName == "" {
			continue
		}

		m.fields = append(m.fields, modelField{
			name:      field.Names[0].Name,
			tag:       tag,
			typeName:  typeName,
			isPointer: isPointer,
		})
	}
	return m
}

// collectFields walks the model graph rooted at typeName and produces one field
// entry per patchable leaf. Nested struct fields recurse; the seen map breaks
// cyclic type references, then releases its entry on the way out so siblings
// can revisit the same type at a different path.
func collectFields(
	models map[string]model,
	buckets []bucket,
	typeName string,
	prefix string,
	selector string,
	guards []string,
	seen map[string]bool,
) []field {
	if seen[typeName] {
		return nil
	}
	m, ok := models[typeName]
	if !ok {
		return nil
	}
	seen[typeName] = true
	defer delete(seen, typeName)

	fields := make([]field, 0, len(m.fields))
	for _, modelField := range m.fields {
		path := modelField.tag
		if prefix != "" {
			path = prefix + "." + modelField.tag
		}

		nextSelector := selector + "." + modelField.name
		nextGuards := guards
		if modelField.isPointer {
			nextGuards = append(append([]string{}, guards...), nextSelector)
		}

		if _, ok := models[modelField.typeName]; ok {
			fields = append(fields, collectFields(models, buckets, modelField.typeName, path, nextSelector, nextGuards, seen)...)
			continue
		}

		fieldBucket, ok := bucketFor(buckets, path)
		if !ok {
			continue
		}

		fields = append(fields, field{
			path:     path,
			key:      modelField.tag,
			selector: nextSelector,
			guards:   guards,
			mapField: fieldBucket.mapField,
		})
	}
	return fields
}

// typeName flattens a type expression into a bare identifier and a flag for
// whether the original spelling was a pointer. Anything else (composite types,
// generics, selector expressions) returns ("", false) so the caller skips it.
func typeName(expr ast.Expr) (string, bool) {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name, false
	case *ast.StarExpr:
		name, _ := typeName(expr.X)
		return name, true
	default:
		return "", false
	}
}

// bucketFor returns the most specific bucket that owns the given dotted path.
// Buckets are pre-sorted by descending prefix length, so the first match wins.
func bucketFor(buckets []bucket, path string) (bucket, bool) {
	for _, bucket := range buckets {
		if bucket.prefix == "" || path == bucket.prefix || strings.HasPrefix(path, bucket.prefix+".") {
			return bucket, true
		}
	}
	return bucket{}, false
}

// mapFields returns the deduplicated bucket map names in lexicographic order so
// the generated `data := patchData{...}` block is stable across runs.
func mapFields(buckets []bucket) []string {
	seen := make(map[string]bool, len(buckets))
	fields := make([]string, 0, len(buckets))
	for _, bucket := range buckets {
		if seen[bucket.mapField] {
			continue
		}
		seen[bucket.mapField] = true
		fields = append(fields, bucket.mapField)
	}
	sort.Strings(fields)
	return fields
}

// nilConditions joins `selector != nil` clauses with `&&`, used by copyRule
// emission to guard a pointer dereference.
func nilConditions(selectors []string) string {
	conditions := make([]string, len(selectors))
	for i, selector := range selectors {
		conditions[i] = selector + " != nil"
	}
	return strings.Join(conditions, " && ")
}
