// Package patchfield implements the "mapgen patch" subcommand: it generates
// per-field patch dispatchers for partial-update handlers with zero
// configuration.
//
//	mapgen patch    # discovers every feature under internal/feature/<f>/
//
// For each feature package it looks for a PatchParams struct — one that carries
// a single struct-typed payload field plus a []string paths field (and usually
// an ID). From the payload struct's `field:"..."`-tagged shape it derives the
// bucket layout (one map per nesting level), the guarded copies for nested
// pointers, and the patchData struct itself, then emits
// internal/feature/<f>/patch_generated.go: the patchData type and a
// patchFields(params PatchParams) patchData dispatcher.
//
// The generated code restricts writes to fields tagged with `field:"..."` in the
// payload struct, so it carries no extra runtime allowlist check.
package patchfield

import (
	"bytes"
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

	"github.com/kitti12911/lib-orm/v4/internal/codegen"
)

const (
	featureDirRel   = "internal/feature"
	outFileName     = "patch_generated.go"
	patchParamsType = "PatchParams"
	functionName    = "patchFields"
	dataType        = "patchData"
)

// field is the fully-resolved info needed to emit one `case` clause.
type field struct {
	path     string
	key      string
	selector string
	guards   []string
	mapField string
}

// model captures one struct type along with the subset of its fields that
// participate in patches (tagged with `field:"..."`).
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

// copyRule expresses a guarded pointer copy emitted before the path switch:
// `if guards... && source != nil { target = *source }`. field/typeName describe
// the corresponding patchData struct field.
type copyRule struct {
	source   string
	target   string
	guards   []string
	field    string
	typeName string
}

// dataStructField is one field of the generated patchData struct.
type dataStructField struct {
	name string
	typ  string
}

// generateSpec is the fully-resolved input to one generation run.
type generateSpec struct {
	file             string
	root             string
	out              string
	packageName      string
	rootSelector     string
	pathsSelector    string
	buckets          []bucket
	copyRules        []copyRule
	dataStructFields []dataStructField
}

// Run discovers every feature package and generates a patch dispatcher for each
// one that declares a PatchParams struct. -C sets the repo root.
func Run(args []string) error {
	fs := flag.NewFlagSet("mapgen patch", flag.ContinueOnError)
	dir := fs.String("C", ".", "repo root directory")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	featureRoot := filepath.Join(*dir, featureDirRel)
	entries, err := os.ReadDir(featureRoot)
	if err != nil {
		return fmt.Errorf("read feature directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		featureDir := filepath.Join(featureRoot, entry.Name())
		spec, ok, err := specFromFeature(featureDir)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := generate(spec); err != nil {
			return fmt.Errorf("generate %s: %w", spec.out, err)
		}
	}
	return nil
}

// specFromFeature derives a generateSpec from a feature package by convention.
// ok is false when the package declares no PatchParams (nothing to generate) or
// is marked //mapgen:ignore.
func specFromFeature(dir string) (generateSpec, bool, error) {
	pkg, ignore, patch, models, err := parseFeature(dir)
	if err != nil {
		return generateSpec{}, false, err
	}
	if ignore || patch == nil {
		return generateSpec{}, false, nil
	}

	buckets, copyRules, dataFields := derivePatchStructure(models, patch, pkg)

	// Sort buckets so the longest prefix wins during path lookup.
	sortedBuckets := append([]bucket(nil), buckets...)
	sort.SliceStable(sortedBuckets, func(i, j int) bool {
		return len(sortedBuckets[i].prefix) > len(sortedBuckets[j].prefix)
	})

	return generateSpec{
		file:             dir,
		root:             patch.payloadType,
		out:              filepath.Join(dir, outFileName),
		packageName:      pkg,
		rootSelector:     "params." + patch.payloadField,
		pathsSelector:    "params." + patch.pathsField,
		buckets:          sortedBuckets,
		copyRules:        copyRules,
		dataStructFields: dataFields,
	}, true, nil
}

// patchParams captures the shape of a discovered PatchParams struct.
type patchParams struct {
	payloadField string // e.g. "User"
	payloadType  string // e.g. "CreateParams"
	pathsField   string // e.g. "Fields"
}

// parseFeature parses every non-test .go file in dir and returns the package
// name, whether the package is //mapgen:ignore'd, the discovered PatchParams (or
// nil), and the `field:"..."`-tagged model graph.
func parseFeature(dir string) (pkg string, ignore bool, patch *patchParams, models map[string]model, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false, nil, nil, fmt.Errorf("read feature directory: %w", err)
	}

	models = make(map[string]model)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == outFileName {
			continue
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if perr != nil {
			return "", false, nil, nil, fmt.Errorf("parse feature file: %w", perr)
		}
		pkg = file.Name.Name
		if file.Doc != nil && directivePresent(file.Doc) {
			ignore = true
		}
		collectStructs(file, models, &patch)
	}
	return pkg, ignore, patch, models, nil
}

func directivePresent(group *ast.CommentGroup) bool {
	for _, c := range group.List {
		if strings.Contains(c.Text, "mapgen:ignore") {
			return true
		}
	}
	return false
}

func collectStructs(file *ast.File, models map[string]model, patch **patchParams) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			strct, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			models[typeSpec.Name.Name] = parseModel(typeSpec.Name.Name, strct)
			if typeSpec.Name.Name == patchParamsType {
				if p := detectPatchParams(strct); p != nil {
					*patch = p
				}
			}
		}
	}
}

// detectPatchParams recognizes a PatchParams struct: exactly one field of a bare
// named struct type (the payload) and one []string field (the paths). Returns
// nil when the shape doesn't match.
func detectPatchParams(strct *ast.StructType) *patchParams {
	var (
		payloadField, payloadType, pathsField string
		payloadCount                          int
	)
	for _, f := range strct.Fields.List {
		if len(f.Names) == 0 {
			continue
		}
		name := f.Names[0].Name
		switch t := f.Type.(type) {
		case *ast.Ident:
			// A bare named type: candidate payload (skip primitives like string).
			if isExportedType(t.Name) {
				payloadField = name
				payloadType = t.Name
				payloadCount++
			}
		case *ast.ArrayType:
			if id, ok := t.Elt.(*ast.Ident); ok && id.Name == "string" && t.Len == nil {
				pathsField = name
			}
		}
	}
	if payloadCount != 1 || payloadField == "" || pathsField == "" {
		return nil
	}
	return &patchParams{payloadField: payloadField, payloadType: payloadType, pathsField: pathsField}
}

func isExportedType(name string) bool {
	return name != "" && name[0] >= 'A' && name[0] <= 'Z'
}

// derivePatchStructure walks the payload struct to produce the bucket layout,
// guarded copies, and patchData struct fields. Buckets are returned in nesting
// order (root first).
func derivePatchStructure(models map[string]model, patch *patchParams, pkg string) ([]bucket, []copyRule, []dataStructField) {
	buckets := []bucket{{prefix: "", mapField: pkg + "Fields"}}
	var copies []copyRule

	seen := map[string]bool{}
	var walk func(typeName, prefix, selector string, guards []string)
	walk = func(typeName, prefix, selector string, guards []string) {
		if seen[typeName] {
			return
		}
		m, ok := models[typeName]
		if !ok {
			return
		}
		seen[typeName] = true
		defer delete(seen, typeName)

		for _, mf := range m.fields {
			if _, isStruct := models[mf.typeName]; !isStruct {
				continue
			}
			path := mf.tag
			if prefix != "" {
				path = prefix + "." + mf.tag
			}
			childSelector := selector + "." + mf.name

			buckets = append(buckets, bucket{prefix: path, mapField: lastSegment(path) + "Fields"})

			if mf.isPointer {
				copies = append(copies, copyRule{
					source:   childSelector,
					target:   "data." + mf.tag,
					guards:   append([]string(nil), guards...),
					field:    mf.tag,
					typeName: mf.typeName,
				})
			}

			childGuards := guards
			if mf.isPointer {
				childGuards = append(append([]string(nil), guards...), childSelector)
			}
			walk(mf.typeName, path, childSelector, childGuards)
		}
	}
	walk(patch.payloadType, "", "params."+patch.payloadField, nil)

	dataFields := make([]dataStructField, 0, len(buckets)+len(copies))
	for _, b := range buckets {
		dataFields = append(dataFields, dataStructField{name: b.mapField, typ: "map[string]any"})
	}
	for _, c := range copies {
		dataFields = append(dataFields, dataStructField{name: c.field, typ: c.typeName})
	}

	return buckets, copies, dataFields
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// generate runs the full pipeline for one spec: collect every patchable field,
// emit Go source, gofmt it, and atomically write it.
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
	if err := codegen.WriteFileAtomic(spec.out, out); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// renderSource produces the un-gofmt'd Go source: the patchData struct followed
// by the dispatcher function.
func renderSource(spec generateSpec, fields []field) []byte {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by mapgen patch; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", spec.packageName)

	fmt.Fprintf(&buf, "type %s struct {\n", dataType)
	for _, f := range spec.dataStructFields {
		fmt.Fprintf(&buf, "\t%s %s\n", f.name, f.typ)
	}
	buf.WriteString("}\n\n")

	fmt.Fprintf(&buf, "func %s(params %s) %s {\n", functionName, patchParamsType, dataType)

	fmt.Fprintf(&buf, "\tdata := %s{\n", dataType)
	for _, mapField := range mapFields(spec.buckets) {
		fmt.Fprintf(&buf, "\t\t%s: make(map[string]any),\n", mapField)
	}
	buf.WriteString("\t}\n\n")

	for _, rule := range spec.copyRules {
		conditions := append(append([]string{}, rule.guards...), rule.source)
		fmt.Fprintf(&buf, "\tif %s {\n", nilConditions(conditions))
		fmt.Fprintf(&buf, "\t\t%s = *%s\n", rule.target, rule.source)
		buf.WriteString("\t}\n\n")
	}

	fmt.Fprintf(&buf, "\tfor _, path := range %s {\n", spec.pathsSelector)
	buf.WriteString("\t\tswitch path {\n")
	for _, f := range fields {
		fmt.Fprintf(&buf, "\t\tcase %q:\n", f.path)
		if guard := combinedNilGuard(f.guards); guard != "" {
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
// expression `a == nil || a.b == nil || ...`; `||` short-circuits so each
// dereference is only evaluated when its ancestors are non-nil.
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

// parseModels parses every non-test .go file in dir and returns each top-level
// struct type indexed by name.
func parseModels(dir string) (map[string]model, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read feature directory: %w", err)
	}

	models := make(map[string]model)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == outFileName {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse model file: %w", err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				strct, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				models[typeSpec.Name.Name] = parseModel(typeSpec.Name.Name, strct)
			}
		}
	}
	return models, nil
}

// parseModel pulls the patch-relevant fields out of one struct: those with both
// a `field:"..."` tag and a resolvable type name (ident or pointer to one).
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
// entry per patchable leaf.
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

// typeName flattens a type expression into a bare identifier and a pointer flag.
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
func bucketFor(buckets []bucket, path string) (bucket, bool) {
	for _, bucket := range buckets {
		if bucket.prefix == "" || path == bucket.prefix || strings.HasPrefix(path, bucket.prefix+".") {
			return bucket, true
		}
	}
	return bucket{}, false
}

// mapFields returns the deduplicated bucket map names in lexicographic order.
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

// nilConditions joins `selector != nil` clauses with `&&`.
func nilConditions(selectors []string) string {
	conditions := make([]string, len(selectors))
	for i, selector := range selectors {
		conditions[i] = selector + " != nil"
	}
	return strings.Join(conditions, " && ")
}
