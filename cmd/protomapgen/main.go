// Command protomapgen generates one-to-one mapping functions between a Go
// struct and a protobuf message. It works for two common shapes:
//
//   - bun ORM models (struct with `bun.BaseModel` or `bun:"col"` tags). Field
//     names are read from the bun column tag, mapping the same way protoc-gen-go
//     would emit them (snake_case column → PascalCase Go identifier).
//   - plain Go structs (e.g. service-layer DTOs like CreateParams). All
//     exported fields participate; field names are matched by identity because
//     both Go struct fields and proto Go-generated fields use PascalCase.
//
// Scalar fields and time.Time map directly. For enums, relations, or any
// non-scalar type, supply a `converters:` entry naming user-written functions.
// Use `exclude:` to skip fields that have no proto counterpart.
//
// The generator can emit one or both directions (`to_proto`, `from_proto`,
// `both`) and supports an `unwrap:` indirection on the from-proto side so a
// request proto like CreateUserRequest{ User *User } can be mapped to a flat
// CreateParams without an extra hand-written hop.
//
// Run as:
//
//	protomapgen -config protomapgen.yaml
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
		fmt.Fprintf(os.Stderr, "protomapgen: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("protomapgen", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to protomapgen YAML config (required)")
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
			return fmt.Errorf("target %s.%s: %w", tgt.GoImport, tgt.GoType, err)
		}
	}
	return nil
}

// direction selects which mapper functions to emit.
type direction int

const (
	dirBoth direction = iota
	dirToProto
	dirFromProto
)

func parseDirection(s string) (direction, error) {
	switch s {
	case "", "both":
		return dirBoth, nil
	case "to_proto":
		return dirToProto, nil
	case "from_proto":
		return dirFromProto, nil
	default:
		return 0, fmt.Errorf("unknown direction %q (must be to_proto, from_proto, or both)", s)
	}
}

// targetConfig is the resolved, defaulted form of one config entry.
type targetConfig struct {
	GoDir         string
	GoImport      string
	GoAlias       string
	GoType        string
	GoSelf        bool // generated file lives in the same package as the Go struct
	ProtoImport   string
	ProtoAlias    string
	ProtoType     string
	Unwrap        string // when set, FromProto reads data from src.Get<Unwrap>()
	Direction     direction
	FuncToProto   string
	FuncFromProto string
	TargetPointer bool // false → return value type with zero-value fallback
	Package       string
	Out           string
	Exclude       map[string]bool
	Converters    map[string]converterPair
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
	GoDir         string                   `yaml:"go_dir"`
	GoImport      string                   `yaml:"go_import"`
	GoAlias       string                   `yaml:"go_alias"`
	GoType        string                   `yaml:"go_type"`
	ProtoImport   string                   `yaml:"proto_import"`
	ProtoAlias    string                   `yaml:"proto_alias"`
	ProtoType     string                   `yaml:"proto_type"`
	Unwrap        string                   `yaml:"unwrap"`
	Direction     string                   `yaml:"direction"`
	FuncToProto   string                   `yaml:"func_to_proto"`
	FuncFromProto string                   `yaml:"func_from_proto"`
	TargetPointer *bool                    `yaml:"target_pointer"`
	Package       string                   `yaml:"package"`
	Out           string                   `yaml:"out"`
	Exclude       []string                 `yaml:"exclude"`
	Converters    map[string]yamlConverter `yaml:"converters"`
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
		{"go_dir", t.GoDir},
		{"go_import", t.GoImport},
		{"go_type", t.GoType},
		{"proto_import", t.ProtoImport},
		{"package", t.Package},
		{"out", t.Out},
	}
	for _, r := range required {
		if r.val == "" {
			return targetConfig{}, fmt.Errorf("%s is required", r.name)
		}
	}

	dir, err := parseDirection(t.Direction)
	if err != nil {
		return targetConfig{}, err
	}

	if t.Unwrap != "" && dir == dirToProto {
		return targetConfig{}, errors.New("unwrap is only valid with direction from_proto or both")
	}

	goAlias := t.GoAlias
	if goAlias == "" {
		goAlias = defaultGoAlias(t.GoImport)
	}
	protoAlias := t.ProtoAlias
	if protoAlias == "" {
		protoAlias = defaultProtoAlias(t.ProtoImport)
	}
	protoType := t.ProtoType
	if protoType == "" {
		protoType = t.GoType
	}

	funcToProto := t.FuncToProto
	if funcToProto == "" {
		funcToProto = t.GoType + "ToProto"
	}
	funcFromProto := t.FuncFromProto
	if funcFromProto == "" {
		funcFromProto = t.GoType + "FromProto"
	}

	targetPointer := true
	if t.TargetPointer != nil {
		targetPointer = *t.TargetPointer
	}

	exclude := make(map[string]bool, len(t.Exclude))
	for _, name := range t.Exclude {
		exclude[name] = true
	}

	converters, err := resolveConverters(t.Converters, dir)
	if err != nil {
		return targetConfig{}, err
	}

	// goSelf is true when the generated file lives in the same Go package
	// as the source struct. Detected by comparing the output directory to
	// the struct directory after cleaning both paths so a trailing slash or
	// "./" prefix doesn't matter. When true, the generator omits the go
	// import and emits bare type names (`CreateParams` instead of
	// `user.CreateParams`) to avoid an import cycle on itself.
	goSelf := filepath.Clean(filepath.Dir(t.Out)) == filepath.Clean(t.GoDir)

	return targetConfig{
		GoDir:         t.GoDir,
		GoImport:      t.GoImport,
		GoAlias:       goAlias,
		GoType:        t.GoType,
		GoSelf:        goSelf,
		ProtoImport:   t.ProtoImport,
		ProtoAlias:    protoAlias,
		ProtoType:     protoType,
		Unwrap:        t.Unwrap,
		Direction:     dir,
		FuncToProto:   funcToProto,
		FuncFromProto: funcFromProto,
		TargetPointer: targetPointer,
		Package:       t.Package,
		Out:           t.Out,
		Exclude:       exclude,
		Converters:    converters,
	}, nil
}

// resolveConverters validates each converter entry against the chosen
// direction (a from_proto-only mapping doesn't need a to_proto func, etc.)
// and returns the resolved map. Split out from resolveTarget to keep that
// function under the workspace cyclomatic-complexity limit.
func resolveConverters(in map[string]yamlConverter, dir direction) (map[string]converterPair, error) {
	out := make(map[string]converterPair, len(in))
	for fieldName, c := range in {
		switch dir {
		case dirToProto:
			if c.ToProto == "" {
				return nil, fmt.Errorf("converter for %q needs to_proto", fieldName)
			}
		case dirFromProto:
			if c.FromProto == "" {
				return nil, fmt.Errorf("converter for %q needs from_proto", fieldName)
			}
		case dirBoth:
			if c.ToProto == "" || c.FromProto == "" {
				return nil, fmt.Errorf("converter for %q needs both to_proto and from_proto", fieldName)
			}
		}
		out[fieldName] = converterPair(c)
	}
	return out, nil
}

func generateTarget(cfg targetConfig) error {
	fields, isBun, err := parseStruct(cfg.GoDir, cfg.GoType)
	if err != nil {
		return err
	}

	src, err := generate(cfg, fields, isBun)
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Out), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	return writeFileAtomic(cfg.Out, src)
}

// kind enumerates the classes of Go field types protomapgen recognizes.
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
	goName  string // Go struct field name, PascalCase
	column  string // bun column tag value (empty for non-bun mode)
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

// parseStruct loads the Go struct identified by typeName from dir. It returns
// the discovered fields and a flag indicating whether the struct is a bun
// model (embeds bun.BaseModel or has any bun:"..." tag). In bun mode only
// bun-tagged fields participate; in plain mode all named exported fields do.
func parseStruct(dir, typeName string) (fs []field, isBun bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, fmt.Errorf("read struct dir %s: %w", dir, err)
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
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if parseErr != nil {
			return nil, false, fmt.Errorf("parse %s: %w", path, parseErr)
		}

		strct, ok := findStruct(file, typeName)
		if !ok {
			continue
		}
		fields, isBunStruct := fieldsFromStruct(strct)
		return fields, isBunStruct, nil
	}

	return nil, false, fmt.Errorf("struct %q not found in %s", typeName, dir)
}

func findStruct(file *ast.File, typeName string) (*ast.StructType, bool) {
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
			return strct, true
		}
	}
	return nil, false
}

func fieldsFromStruct(strct *ast.StructType) (fs []field, isBun bool) {
	// First pass: decide bun vs plain by looking at tags + embed.
	for _, f := range strct.Fields.List {
		if structTag(f).Get("bun") != "" {
			isBun = true
			break
		}
		// Detect embedded bun.BaseModel (no field name).
		if len(f.Names) == 0 {
			if sel, ok := f.Type.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "bun" && sel.Sel.Name == "BaseModel" {
					isBun = true
					break
				}
			}
		}
	}

	for _, f := range strct.Fields.List {
		// Skip embedded fields. In bun mode bun.BaseModel only carries
		// table:/alias: metadata, not a field; in plain mode we ignore
		// embeds because matching them to a proto field is ambiguous.
		if len(f.Names) == 0 {
			continue
		}
		name := f.Names[0].Name
		if !isExported(name) {
			continue
		}

		tag := structTag(f)
		bunTag := tag.Get("bun")

		if isBun {
			if bunTag == "" {
				// In bun mode, skip non-bun-tagged fields. Mixing tagged
				// and untagged fields on the same struct is unusual;
				// supporting it would need disambiguation we don't need
				// right now.
				continue
			}
			if strings.Contains(bunTag, "rel:") {
				fs = append(fs, field{
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
			fs = append(fs, field{
				goName:  name,
				column:  column,
				kind:    k,
				rawType: raw,
			})
			continue
		}

		// Plain mode: use the Go field name as the proto identifier
		// (column is left empty so protoFieldRef falls back to goName).
		k, raw := classify(f.Type)
		fs = append(fs, field{
			goName:  name,
			kind:    k,
			rawType: raw,
		})
	}

	sort.Slice(fs, func(i, j int) bool { return fs[i].goName < fs[j].goName })
	return fs, isBun
}

func isExported(name string) bool {
	if name == "" {
		return false
	}
	r := name[0]
	return r >= 'A' && r <= 'Z'
}

func classify(expr ast.Expr) (k kind, raw string) {
	switch t := expr.(type) {
	case *ast.Ident:
		if scalarTypes[t.Name] {
			return kindScalar, t.Name
		}
		return kindUnknown, t.Name
	case *ast.StarExpr:
		if ident, ok := t.X.(*ast.Ident); ok {
			if scalarTypes[ident.Name] {
				return kindPtrScalar, "*" + ident.Name
			}
			// Pointer to a user-defined type: treat as relation in plain
			// mode (needs a converter or exclude). Bun mode would have
			// caught this earlier via the rel: tag.
			return kindRelation, "*" + ident.Name
		}
		return kindRelation, "*" + exprString(t.X)
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
// protoc-gen-go would emit: title-case each underscore-delimited word.
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

// defaultGoAlias chooses a stable alias for the Go struct's package when the
// user does not supply one. The package's last path segment is the standard
// Go convention; we fall back to "src" only if the import path is unusable.
func defaultGoAlias(importPath string) string {
	if importPath == "" {
		return "src"
	}
	segs := strings.Split(importPath, "/")
	last := segs[len(segs)-1]
	if last == "" {
		return "src"
	}
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

func generate(cfg targetConfig, fields []field, _ bool) ([]byte, error) {
	var buf bytes.Buffer

	buf.WriteString("// Code generated by protomapgen; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", cfg.Package)

	buf.WriteString("import (\n")
	if !cfg.GoSelf {
		fmt.Fprintf(&buf, "\t%s %q\n", cfg.GoAlias, cfg.GoImport)
	}
	fmt.Fprintf(&buf, "\t%s %q\n", cfg.ProtoAlias, cfg.ProtoImport)
	if needsTimestamppb(fields, cfg) {
		fmt.Fprintf(&buf, "\t%q\n", timestamppbImport)
	}
	buf.WriteString(")\n\n")

	if cfg.Direction == dirBoth || cfg.Direction == dirToProto {
		writeToProto(&buf, cfg, fields)
		buf.WriteString("\n")
	}
	if cfg.Direction == dirBoth || cfg.Direction == dirFromProto {
		writeFromProto(&buf, cfg, fields)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w\n--- source ---\n%s", err, buf.String())
	}
	return out, nil
}

func needsTimestamppb(fields []field, cfg targetConfig) bool {
	if cfg.Direction == dirFromProto {
		// FromProto reads from a *timestamppb.Timestamp; the import is on
		// the proto side, not ours. We only need the import when ToProto
		// constructs new timestamppb values.
		return false
	}
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

// outerProtoType is the proto type the generated function takes as input on
// the FromProto side (and outputs on the ToProto side, currently always
// equal to ProtoType because unwrap is from_proto-only).
func outerProtoType(cfg targetConfig) string {
	return cfg.ProtoType
}

// goTypeRef returns the bare Go type name when the generated file lives in
// the same package as the source struct (e.g. `CreateParams`) and the
// qualified name otherwise (e.g. `user.CreateParams`). Used so generated
// files in-package don't self-import.
func goTypeRef(cfg targetConfig) string {
	if cfg.GoSelf {
		return cfg.GoType
	}
	return cfg.GoAlias + "." + cfg.GoType
}

func writeToProto(buf *bytes.Buffer, cfg targetConfig, fields []field) {
	srcType := goTypeRef(cfg)
	dstType := fmt.Sprintf("%s.%s", cfg.ProtoAlias, outerProtoType(cfg))

	fmt.Fprintf(buf, "// %s maps a %s to its proto representation.\n", cfg.FuncToProto, cfg.GoType)
	buf.WriteString("// Generated by protomapgen — hand edits will be overwritten.\n")
	fmt.Fprintf(buf, "func %s(src *%s) *%s {\n", cfg.FuncToProto, srcType, dstType)
	buf.WriteString("\tif src == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(buf, "\tdst := &%s{\n", dstType)

	for _, f := range fields {
		if cfg.Exclude[f.goName] {
			continue
		}
		if conv, ok := cfg.Converters[f.goName]; ok && conv.ToProto != "" {
			fmt.Fprintf(buf, "\t\t%s: %s(src.%s),\n", protoFieldRef(f), conv.ToProto, f.goName)
			continue
		}
		switch {
		case isDirectMappable(f.kind):
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
	outerType := fmt.Sprintf("%s.%s", cfg.ProtoAlias, outerProtoType(cfg))

	// returnType controls the declared return type and the zero-value
	// expression used for nil inputs. target_pointer=false yields the value
	// shape used by service-layer params structs (CreateParams{}, etc).
	goType := goTypeRef(cfg)
	var returnType, returnZero, openLiteral string
	if cfg.TargetPointer {
		returnType = "*" + goType
		returnZero = "nil"
		openLiteral = "&" + goType + "{"
	} else {
		returnType = goType
		returnZero = goType + "{}"
		openLiteral = goType + "{"
	}

	fmt.Fprintf(buf, "// %s maps a proto message back to %s.\n", cfg.FuncFromProto, cfg.GoType)
	buf.WriteString("// Generated by protomapgen — hand edits will be overwritten.\n")
	fmt.Fprintf(buf, "func %s(in *%s) %s {\n", cfg.FuncFromProto, outerType, returnType)
	fmt.Fprintf(buf, "\tif in == nil {\n\t\treturn %s\n\t}\n", returnZero)

	// readVar is the identifier the body uses to access fields. With unwrap,
	// we dereference into the sub-message and guard nil. Without unwrap, we
	// read directly from the outer argument.
	readVar := "in"
	if cfg.Unwrap != "" {
		readVar = "src"
		fmt.Fprintf(buf, "\tsrc := in.Get%s()\n", cfg.Unwrap)
		fmt.Fprintf(buf, "\tif src == nil {\n\t\treturn %s\n\t}\n", returnZero)
	}

	fmt.Fprintf(buf, "\tdst := %s\n", openLiteral)

	for _, f := range fields {
		if cfg.Exclude[f.goName] {
			continue
		}
		if conv, ok := cfg.Converters[f.goName]; ok && conv.FromProto != "" {
			fmt.Fprintf(buf, "\t\t%s: %s(%s.%s),\n", f.goName, conv.FromProto, readVar, protoFieldRef(f))
			continue
		}
		switch {
		case isDirectMappable(f.kind):
			fmt.Fprintf(buf, "\t\t%s: %s.%s,\n", f.goName, readVar, protoFieldRef(f))
		case f.kind == kindTime:
			// AsTime is nil-safe — returns the zero epoch when unset.
			fmt.Fprintf(buf, "\t\t%s: %s.%s.AsTime(),\n", f.goName, readVar, protoFieldRef(f))
		default:
			fmt.Fprintf(buf, "\t\t// TODO: %s (%s)\n", f.goName, todoSuffix(f))
		}
	}

	buf.WriteString("\t}\n")
	buf.WriteString("\treturn dst\n")
	buf.WriteString("}\n")
}

// protoFieldRef returns the Go identifier of the proto field corresponding to
// a Go struct field. Bun-mode fields carry a column tag whose snake_case
// becomes PascalCase; plain-mode fields use the Go field name verbatim
// because proto-generated Go code is already PascalCase.
func protoFieldRef(f field) string {
	if f.column == "" {
		return f.goName
	}
	return protoGoName(f.column)
}

func isDirectMappable(k kind) bool {
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
	tmp, err := os.CreateTemp(dir, ".protomapgen-*")
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
