// Command bunprotogen reads a YAML config of (bun model, proto message)
// pairs and emits a "dumb" pair of proto<->bun mapping functions for each.
//
// The generator handles fields it can be confident about: scalar bun
// columns whose Go field name lines up with a generated proto Go field
// name via the snake_case column tag, plus time.Time <-> timestamppb.
// For everything else (enums, relations, custom types) the config can
// name a user-provided converter function — bunprotogen will call it by
// name. Fields without a converter fall back to a TODO comment.
//
// Run as:
//
//	bunprotogen -config bunprotogen.yaml
//
// See the package README for the full schema.
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

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "bunprotogen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("bunprotogen", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to bunprotogen YAML config (required)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	for _, tgt := range cfg {
		if err := generateTarget(tgt); err != nil {
			return fmt.Errorf("target %s.%s: %w", tgt.BunImport, tgt.BunType, err)
		}
	}
	return nil
}

// targetConfig is the resolved, defaulted form of one config entry.
type targetConfig struct {
	BunDir      string
	BunImport   string
	BunAlias    string
	BunType     string
	ProtoImport string
	ProtoAlias  string
	ProtoType   string
	Package     string
	Out         string
	Exclude     map[string]bool
	Converters  map[string]converterPair
}

type converterPair struct {
	ToProto   string
	FromProto string
}

// yamlConfig mirrors the on-disk schema.
type yamlConfig struct {
	Targets []yamlTarget `yaml:"targets"`
}

type yamlTarget struct {
	BunDir      string                   `yaml:"bun_dir"`
	BunImport   string                   `yaml:"bun_import"`
	BunAlias    string                   `yaml:"bun_alias"`
	BunType     string                   `yaml:"bun_type"`
	ProtoImport string                   `yaml:"proto_import"`
	ProtoAlias  string                   `yaml:"proto_alias"`
	ProtoType   string                   `yaml:"proto_type"`
	Package     string                   `yaml:"package"`
	Out         string                   `yaml:"out"`
	Exclude     []string                 `yaml:"exclude"`
	Converters  map[string]yamlConverter `yaml:"converters"`
}

type yamlConverter struct {
	ToProto   string `yaml:"to_proto"`
	FromProto string `yaml:"from_proto"`
}

func loadConfig(path string) ([]targetConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yamlConfig
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if len(doc.Targets) == 0 {
		return nil, errors.New("config has no targets")
	}

	out := make([]targetConfig, 0, len(doc.Targets))
	for i, t := range doc.Targets {
		resolved, err := resolveTarget(t)
		if err != nil {
			return nil, fmt.Errorf("targets[%d]: %w", i, err)
		}
		out = append(out, resolved)
	}
	return out, nil
}

func resolveTarget(t yamlTarget) (targetConfig, error) {
	required := []struct{ name, val string }{
		{"bun_dir", t.BunDir},
		{"bun_import", t.BunImport},
		{"bun_type", t.BunType},
		{"proto_import", t.ProtoImport},
		{"package", t.Package},
		{"out", t.Out},
	}
	for _, r := range required {
		if r.val == "" {
			return targetConfig{}, fmt.Errorf("%s is required", r.name)
		}
	}

	bunAlias := t.BunAlias
	if bunAlias == "" {
		bunAlias = "database"
	}
	protoAlias := t.ProtoAlias
	if protoAlias == "" {
		protoAlias = defaultProtoAlias(t.ProtoImport)
	}
	protoType := t.ProtoType
	if protoType == "" {
		protoType = t.BunType
	}

	exclude := make(map[string]bool, len(t.Exclude))
	for _, name := range t.Exclude {
		exclude[name] = true
	}

	converters := make(map[string]converterPair, len(t.Converters))
	for field, c := range t.Converters {
		if c.ToProto == "" || c.FromProto == "" {
			return targetConfig{}, fmt.Errorf("converter for %q needs both to_proto and from_proto", field)
		}
		converters[field] = converterPair(c)
	}

	return targetConfig{
		BunDir:      t.BunDir,
		BunImport:   t.BunImport,
		BunAlias:    bunAlias,
		BunType:     t.BunType,
		ProtoImport: t.ProtoImport,
		ProtoAlias:  protoAlias,
		ProtoType:   protoType,
		Package:     t.Package,
		Out:         t.Out,
		Exclude:     exclude,
		Converters:  converters,
	}, nil
}

func generateTarget(cfg targetConfig) error {
	fields, err := parseModel(cfg.BunDir, cfg.BunType)
	if err != nil {
		return err
	}

	src, err := generate(cfg, fields)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Out), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return writeFileAtomic(cfg.Out, src)
}

// kind enumerates the classes of bun field types bunprotogen recognizes.
type kind int

const (
	kindUnknown kind = iota
	kindScalar
	kindPtrScalar
	kindByteSlice
	kindRelation
	kindTime // time.Time ↔ *timestamppb.Timestamp
)

const timestamppbImport = "google.golang.org/protobuf/types/known/timestamppb"

type field struct {
	goName  string // bun struct field name, PascalCase
	column  string // bun column tag value, snake_case
	kind    kind
	rawType string // human-readable type for TODO comments
}

var scalarTypes = map[string]bool{
	"string":  true,
	"bool":    true,
	"int32":   true,
	"int64":   true,
	"uint32":  true,
	"uint64":  true,
	"float32": true,
	"float64": true,
}

func parseModel(dir, typeName string) ([]field, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read model dir %s: %w", dir, err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		fields, ok := extractFields(file, typeName)
		if ok {
			return fields, nil
		}
	}

	return nil, fmt.Errorf("model type %q not found in %s", typeName, dir)
}

func extractFields(file *ast.File, typeName string) ([]field, bool) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != typeName {
				continue
			}
			strct, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			return fieldsFromStruct(strct), true
		}
	}
	return nil, false
}

func fieldsFromStruct(strct *ast.StructType) []field {
	var out []field
	for _, f := range strct.Fields.List {
		tag := structTag(f)
		bunTag := tag.Get("bun")
		if bunTag == "" {
			continue
		}
		// Embedded bun.BaseModel carries table:/alias: but no field name.
		if len(f.Names) == 0 {
			continue
		}
		name := f.Names[0].Name

		if strings.Contains(bunTag, "rel:") {
			out = append(out, field{
				goName:  name,
				kind:    kindRelation,
				rawType: exprString(f.Type),
			})
			continue
		}

		column := strings.Split(bunTag, ",")[0]
		if column == "" {
			continue
		}

		k, raw := classify(f.Type)
		out = append(out, field{
			goName:  name,
			column:  column,
			kind:    k,
			rawType: raw,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].goName < out[j].goName })
	return out
}

func classify(expr ast.Expr) (k kind, raw string) {
	switch t := expr.(type) {
	case *ast.Ident:
		if scalarTypes[t.Name] {
			return kindScalar, t.Name
		}
		return kindUnknown, t.Name
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok && scalarTypes[ident.Name] {
			return kindPtrScalar, "*" + ident.Name
		}
		return kindUnknown, "*" + exprString(t.X)
	case *ast.ArrayType:
		if ident, ok := t.Elt.(*ast.Ident); ok && ident.Name == "byte" {
			return kindByteSlice, "[]byte"
		}
		return kindUnknown, "[]" + exprString(t.Elt)
	case *ast.SelectorExpr:
		if pkg, ok := t.X.(*ast.Ident); ok && pkg.Name == "time" && t.Sel.Name == "Time" {
			return kindTime, "time.Time"
		}
		return kindUnknown, exprString(t)
	default:
		return kindUnknown, exprString(t)
	}
}

func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	default:
		return "?"
	}
}

func structTag(f *ast.Field) reflect.StructTag {
	if f.Tag == nil {
		return ""
	}
	return reflect.StructTag(strings.Trim(f.Tag.Value, "`"))
}

// protoGoName converts a snake_case proto field name to the Go identifier
// that protoc-gen-go would emit: title-case each underscore-delimited word.
// "user_id" -> "UserId", "id" -> "Id", "display_name" -> "DisplayName".
func protoGoName(column string) string {
	parts := strings.Split(column, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

func defaultProtoAlias(importPath string) string {
	if importPath == "" {
		return ""
	}
	segs := strings.Split(importPath, "/")
	last := segs[len(segs)-1]
	if len(segs) >= 2 && looksLikeVersion(last) {
		return segs[len(segs)-2] + last
	}
	return last
}

func looksLikeVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func generate(cfg targetConfig, fields []field) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString("// Code generated by bunprotogen; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", cfg.Package)

	fmt.Fprintf(&buf, "import (\n")
	fmt.Fprintf(&buf, "\t%s %q\n", cfg.BunAlias, cfg.BunImport)
	fmt.Fprintf(&buf, "\t%s %q\n", cfg.ProtoAlias, cfg.ProtoImport)
	if needsTimestamppb(fields, cfg) {
		fmt.Fprintf(&buf, "\t%q\n", timestamppbImport)
	}
	fmt.Fprintf(&buf, ")\n\n")

	writeToProto(&buf, cfg, fields)
	buf.WriteString("\n")
	writeFromProto(&buf, cfg, fields)

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w\n--- source ---\n%s", err, buf.String())
	}
	return out, nil
}

// needsTimestamppb returns true when at least one time.Time field is
// emitted with the timestamppb default (i.e. has no converter override
// and is not excluded).
func needsTimestamppb(fields []field, cfg targetConfig) bool {
	for _, f := range fields {
		if cfg.Exclude[f.goName] {
			continue
		}
		if _, hasConverter := cfg.Converters[f.goName]; hasConverter {
			continue
		}
		if f.kind == kindTime {
			return true
		}
	}
	return false
}

func writeToProto(buf *bytes.Buffer, cfg targetConfig, fields []field) {
	fmt.Fprintf(buf, "// %sToProto maps a bun model to its proto representation. Scalar fields\n", cfg.BunType)
	buf.WriteString("// with matching names are assigned directly; configured converters bridge\n")
	buf.WriteString("// non-scalar fields; everything else becomes a TODO comment.\n")
	fmt.Fprintf(buf, "func %sToProto(src *%s.%s) *%s.%s {\n", cfg.BunType, cfg.BunAlias, cfg.BunType, cfg.ProtoAlias, cfg.ProtoType)
	buf.WriteString("\tif src == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(buf, "\tdst := &%s.%s{\n", cfg.ProtoAlias, cfg.ProtoType)

	for _, f := range fields {
		if cfg.Exclude[f.goName] {
			continue
		}
		if conv, ok := cfg.Converters[f.goName]; ok {
			fmt.Fprintf(buf, "\t\t%s: %s(src.%s),\n", protoFieldRef(f), conv.ToProto, f.goName)
			continue
		}
		switch {
		case isMappable(f.kind):
			fmt.Fprintf(buf, "\t\t%s: src.%s,\n", protoFieldRef(f), f.goName)
		case f.kind == kindTime:
			fmt.Fprintf(buf, "\t\t%s: timestamppb.New(src.%s),\n", protoFieldRef(f), f.goName)
		default:
			fmt.Fprintf(buf, "\t\t// TODO: %s (%s)\n", f.goName, todoSuffix(f))
		}
	}

	buf.WriteString("\t}\n")
	buf.WriteString("\treturn dst\n")
	buf.WriteString("}\n")
}

func writeFromProto(buf *bytes.Buffer, cfg targetConfig, fields []field) {
	fmt.Fprintf(buf, "// %sFromProto maps a proto message back to its bun model. See %sToProto\n", cfg.BunType, cfg.BunType)
	buf.WriteString("// for the field-mapping contract.\n")
	fmt.Fprintf(buf, "func %sFromProto(src *%s.%s) *%s.%s {\n", cfg.BunType, cfg.ProtoAlias, cfg.ProtoType, cfg.BunAlias, cfg.BunType)
	buf.WriteString("\tif src == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(buf, "\tdst := &%s.%s{\n", cfg.BunAlias, cfg.BunType)

	for _, f := range fields {
		if cfg.Exclude[f.goName] {
			continue
		}
		if conv, ok := cfg.Converters[f.goName]; ok {
			fmt.Fprintf(buf, "\t\t%s: %s(src.%s),\n", f.goName, conv.FromProto, protoFieldRef(f))
			continue
		}
		switch {
		case isMappable(f.kind):
			fmt.Fprintf(buf, "\t\t%s: src.%s,\n", f.goName, protoFieldRef(f))
		case f.kind == kindTime:
			// (*timestamppb.Timestamp).AsTime() is nil-safe — returns the
			// zero epoch when the proto field is unset.
			fmt.Fprintf(buf, "\t\t%s: src.%s.AsTime(),\n", f.goName, protoFieldRef(f))
		default:
			fmt.Fprintf(buf, "\t\t// TODO: %s (%s)\n", f.goName, todoSuffix(f))
		}
	}

	buf.WriteString("\t}\n")
	buf.WriteString("\treturn dst\n")
	buf.WriteString("}\n")
}

// protoFieldRef returns the Go identifier of the proto field corresponding
// to a bun field. Relations don't carry a column tag, so they default to
// the bun field name (which is what protoc-gen-go would emit for a nested
// message of the same name).
func protoFieldRef(f field) string {
	if f.column == "" {
		return f.goName
	}
	return protoGoName(f.column)
}

func isMappable(k kind) bool {
	return k == kindScalar || k == kindPtrScalar || k == kindByteSlice
}

func todoSuffix(f field) string {
	if f.kind == kindRelation {
		return f.rawType + ", relation"
	}
	return f.rawType
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bunprotogen-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup; rename above already handled the success path.

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // already in error path, primary error is reported.
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck // already in error path, primary error is reported.
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
