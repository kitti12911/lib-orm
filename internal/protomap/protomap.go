// Package protomap implements the "mapgen proto" subcommand: it generates
// one-to-one mapping functions between a Go struct and a protobuf message. It
// works for two common shapes:
//
//   - bun ORM models (struct with `bun.BaseModel` or `bun:"col"` tags). Field
//     names are read from the bun column tag, mapping the same way protoc-gen-go
//     would emit them (snake_case column -> PascalCase Go identifier).
//   - plain Go structs (e.g. service-layer DTOs like CreateParams). All
//     exported fields participate; field names are matched by identity because
//     both Go struct fields and proto Go-generated fields use PascalCase.
//
// Scalar fields and time.Time map directly. For enums, relations, or any
// non-scalar type, supply a `converters:` entry naming user-written functions.
// Use `exclude:` to skip fields that have no proto counterpart.
//
// In plain mode, `field_names: protoc` runs each Go field name through
// protoc-gen-go's algorithm so initialism fields line up (SkillID <-> SkillId);
// `field_overrides:` gives an explicit per-field proto identifier for the rest.
//
// The generator can emit one or both directions (`to_proto`, `from_proto`,
// `both`) and supports an `unwrap:` indirection on the from-proto side so a
// request proto like CreateUserRequest{ User *User } can be mapped to a flat
// CreateParams without an extra hand-written hop.
//
// Run as:
//
//	mapgen proto -config protomapgen.yaml
//
// See the package README for the full schema.
package protomap

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
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kitti12911/lib-orm/v3/internal/codegen"
)

// Run executes the proto mapper generation described by the -config flag.
func Run(args []string) error {
	fs := flag.NewFlagSet("mapgen proto", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to protomapgen.yaml config (required)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}

	targets, enums, err := loadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Parse each target's source struct, then group by Out so that
	// targets and enums sharing an output path are merged into a single file.
	// The order within a group preserves the YAML order (targets then enums) so
	// the resulting file is deterministic and reviewable.
	groups := map[string][]genMember{}
	var outOrder []string
	addOut := func(out string) {
		if _, seen := groups[out]; !seen {
			outOrder = append(outOrder, out)
		}
	}
	for _, tgt := range targets {
		fields, isBun, parseErr := parseStruct(tgt.GoDir, tgt.GoType)
		if parseErr != nil {
			return fmt.Errorf("target %s.%s: %w", tgt.GoImport, tgt.GoType, parseErr)
		}
		addOut(tgt.Out)
		groups[tgt.Out] = append(groups[tgt.Out], genMember{cfg: tgt, fields: fields, isBun: isBun})
	}
	for _, e := range enums {
		addOut(e.Out)
		groups[e.Out] = append(groups[e.Out], genMember{enum: &e})
	}

	for _, out := range outOrder {
		members := groups[out]
		if err := validateGroup(members); err != nil {
			return fmt.Errorf("group %s: %w", out, err)
		}
		src, err := renderGroup(members)
		if err != nil {
			return fmt.Errorf("group %s: %w", out, err)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", out, err)
		}
		if err := codegen.WriteFileAtomic(out, src); err != nil {
			return fmt.Errorf("write %s: %w", out, err)
		}
	}
	return nil
}

// genMember is one declaration in a merged-group file: either a struct mapper
// (cfg/fields/isBun) or, when enum is non-nil, a generated enum switch function.
type genMember struct {
	cfg    targetConfig
	fields []field
	isBun  bool
	enum   *enumConfig
}

// memberPackage returns the Go package the member's generated code belongs to.
func memberPackage(m genMember) string {
	if m.enum != nil {
		return m.enum.Package
	}
	return m.cfg.Package
}

// importEntry is an alias+path pair used to deduplicate imports across
// merged-group members. alias is empty for unaliased imports such as
// timestamppb.
type importEntry struct {
	alias string
	path  string
}

// validateGroup rejects target collisions that would produce a malformed
// merged file: members sharing an Out must declare the same package, and
// must not assign different aliases to the same import path.
func validateGroup(members []genMember) error {
	if len(members) == 0 {
		return nil
	}
	pkg := memberPackage(members[0])
	for _, m := range members[1:] {
		if mp := memberPackage(m); mp != pkg {
			return fmt.Errorf("targets share out but disagree on package: %q vs %q", pkg, mp)
		}
	}
	aliasByPath := map[string]string{}
	for _, m := range members {
		entries := importEntries(m)
		for _, e := range entries {
			if prior, ok := aliasByPath[e.path]; ok && prior != e.alias {
				return fmt.Errorf("import %q has conflicting aliases %q and %q across targets", e.path, prior, e.alias)
			}
			aliasByPath[e.path] = e.alias
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

// fieldNameMode controls how a plain-mode (non-bun) Go field name is mapped to
// its proto-generated Go identifier. Bun-mode fields ignore this: they always
// derive the proto name from the column tag.
type fieldNameMode int

const (
	// namesIdentity uses the Go field name verbatim (the historical default):
	// it assumes the source struct field already matches the proto field, e.g.
	// CreateParams.Name <-> proto Name.
	namesIdentity fieldNameMode = iota
	// namesProtoc runs the Go field name through protoc-gen-go's algorithm so
	// initialism fields line up: SkillID -> SkillId, ID -> Id, HTTPURL -> HttpUrl.
	namesProtoc
)

func parseFieldNames(s string) (fieldNameMode, error) {
	switch s {
	case "", "identity":
		return namesIdentity, nil
	case "protoc":
		return namesProtoc, nil
	default:
		return 0, fmt.Errorf("unknown field_names %q (must be identity or protoc)", s)
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
	TargetPointer bool // false -> return value type with zero-value fallback
	// SourcePointer controls the to_proto source parameter. true (default)
	// takes `src *T` with a nil guard; false takes `src T` by value and omits
	// the guard, matching call sites that map a value struct (e.g. a huma
	// request body) directly.
	SourcePointer bool
	// TimeFromProto, when set, names the helper applied to time.Time fields on
	// the from_proto side (e.g. protoutil.TimeFromProto) instead of the bare
	// .AsTime() call. The two differ on unset timestamps: .AsTime() yields the
	// 1970 epoch while a helper can return the time.Time zero value. Pair it
	// with an `imports:` entry for the helper's package.
	TimeFromProto string
	// Getters, on the from_proto side, reads value-typed proto fields through
	// their generated accessor (in.GetX()) instead of direct field access
	// (in.X). The accessor dereferences a proto3-optional scalar to its value
	// type, so an optional proto field maps cleanly onto a non-pointer Go field.
	// Pointer-scalar targets keep direct access so the pointer is preserved.
	Getters bool
	// WrapOutput, when set, names a huma-style envelope type (e.g.
	// ReviewSubmissionOutput) whose WrapField holds the mapped GoType value. The
	// from_proto function then returns *WrapOutput, returning an empty (non-nil)
	// envelope for a nil input and assigning the mapped body to out.<WrapField>.
	WrapOutput string
	WrapField  string // envelope field receiving the body; defaults to "Body"
	Package    string
	Out        string
	Exclude    map[string]bool
	Converters map[string]converterPair
	// FieldNames controls plain-mode proto field-name derivation.
	FieldNames fieldNameMode
	// FieldOverrides maps a Go field name to an explicit proto-generated Go
	// identifier, taking precedence over FieldNames for irregular cases.
	FieldOverrides map[string]string
	// ExtraArgs are non-proto parameters injected into the from_proto signature.
	ExtraArgs []extraArg
	// Imports are extra import paths the generated file needs (e.g. the package
	// of an ExtraArg type such as fieldmaskpb).
	Imports []importEntry
	// ConverterImport is the import path auto-added when this target emits a
	// converter.Slice/SliceDeref call (i.e. it has a repeated converter). Set
	// from the top-level `converter_import:` so callers needn't list it manually.
	ConverterImport string
}

// usesConverter reports whether the generated code references the converter
// package, which happens for any repeated converter (converter.Slice/SliceDeref).
func (c targetConfig) usesConverter() bool {
	for _, conv := range c.Converters {
		if conv.Repeated {
			return true
		}
	}
	return false
}

type converterPair struct {
	ToProto   string
	FromProto string
	// WholeInput, on the from_proto side, calls FromProto with the entire input
	// message instead of a single proto field (e.g. a nested struct filled by
	// another generated mapper). Ignored on the to_proto side.
	WholeInput bool
	// Repeated maps a slice field element-by-element: the converter names the
	// per-element mapper, and the generator emits an append loop over the field.
	Repeated bool
	// ElementPtr passes &src.Field[i] (rather than src.Field[i]) to the
	// per-element to_proto mapper. Ignored on the from_proto side, where proto
	// repeated elements are already pointers.
	ElementPtr bool
	// Deref, on a from_proto repeated converter inside a wrap_output body, maps
	// the slice via converter.SliceDeref (the element mapper returns *D and nil
	// results are skipped) rather than converter.Slice (element mapper returns D).
	Deref bool
}

// extraArg is a non-proto parameter injected into a from_proto function
// signature whose value populates one target field. It lets the generator emit
// wrappers like updateParamsFromProto(id string, in *Proto) that combine the
// proto with caller-supplied data (an ID, a field mask, ...).
type extraArg struct {
	name   string // parameter name
	typ    string // Go type for the signature
	field  string // target struct field it assigns
	expr   string // value expression; defaults to name
	before bool   // emitted before the proto `in` parameter when true
}

// enumConfig generates a standalone switch function converting, in one
// direction, between a Go value (a typed enum constant or a string) and a proto
// enum. The generated function is typically referenced from a field converter.
type enumConfig struct {
	Func        string
	Direction   direction
	ProtoImport string
	ProtoAlias  string
	ProtoType   string
	GoType      string // non-proto side: "string" or a named Go enum type
	Package     string
	Out         string
	// Default is the default-case value: a proto value suffix for to_proto, or a
	// Go expression for from_proto (empty means the GoType zero value).
	Default string
	Values  []enumValue
}

// enumValue pairs a Go side (a bare constant, or string content when GoType is
// "string") with a proto enum value suffix such as EXAM_CATEGORY_SUBJECT.
type enumValue struct {
	goExpr   string
	protoVal string
}

// yamlConfig mirrors the on-disk schema.
type yamlConfig struct {
	// Module is the Go module path; when set, a target's go_import defaults to
	// module + "/" + go_dir, so per-target go_import lines can be dropped.
	Module   string       `yaml:"module"`
	Defaults yamlDefaults `yaml:"defaults"`
	// ConverterImport, when set, is auto-added to any target that emits a
	// converter.Slice/SliceDeref call, so per-target imports can omit it.
	ConverterImport string               `yaml:"converter_import"`
	Groups          map[string]yamlGroup `yaml:"groups"`
	Targets         []yamlTarget         `yaml:"targets"`
	Enums           []yamlEnum           `yaml:"enums"`
}

// yamlGroup declares the fields shared by a set of targets once (typically all
// targets in one go_dir/proto package). A target opts in with `group: <name>`
// and inherits any field it leaves empty; the group's imports merge into the
// target's. Explicit target values always win.
type yamlGroup struct {
	GoDir         string   `yaml:"go_dir"`
	GoImport      string   `yaml:"go_import"`
	ProtoImport   string   `yaml:"proto_import"`
	ProtoAlias    string   `yaml:"proto_alias"`
	Package       string   `yaml:"package"`
	Direction     string   `yaml:"direction"`
	FieldNames    string   `yaml:"field_names"`
	TimeFromProto string   `yaml:"time_from_proto"`
	Out           string   `yaml:"out"`
	Imports       []string `yaml:"imports"`
}

// merge fills a target's empty inheritable fields from its group header and
// prepends the group's shared imports. Explicit target values always win.
func (g yamlGroup) merge(t yamlTarget) yamlTarget {
	or := func(v, fallback string) string {
		if v == "" {
			return fallback
		}
		return v
	}
	t.GoDir = or(t.GoDir, g.GoDir)
	t.GoImport = or(t.GoImport, g.GoImport)
	t.ProtoImport = or(t.ProtoImport, g.ProtoImport)
	t.ProtoAlias = or(t.ProtoAlias, g.ProtoAlias)
	t.Package = or(t.Package, g.Package)
	t.Direction = or(t.Direction, g.Direction)
	t.FieldNames = or(t.FieldNames, g.FieldNames)
	t.TimeFromProto = or(t.TimeFromProto, g.TimeFromProto)
	t.Out = or(t.Out, g.Out)
	if len(g.Imports) > 0 {
		merged := append([]string(nil), g.Imports...)
		seen := make(map[string]bool, len(g.Imports))
		for _, imp := range g.Imports {
			seen[imp] = true
		}
		for _, imp := range t.Imports {
			if !seen[imp] {
				merged = append(merged, imp)
			}
		}
		t.Imports = merged
	}
	return t
}

// mergeEnum fills an enum's empty inheritable fields from its group header so a
// package's enum converters share the proto package/alias and out path with its
// message targets. Explicit enum values always win.
func (g yamlGroup) mergeEnum(e yamlEnum) yamlEnum {
	or := func(v, fallback string) string {
		if v == "" {
			return fallback
		}
		return v
	}
	e.GoDir = or(e.GoDir, g.GoDir)
	e.ProtoImport = or(e.ProtoImport, g.ProtoImport)
	e.ProtoAlias = or(e.ProtoAlias, g.ProtoAlias)
	e.Package = or(e.Package, g.Package)
	e.Direction = or(e.Direction, g.Direction)
	e.Out = or(e.Out, g.Out)
	return e
}

// yamlDefaults supplies fall-back values merged into every target/enum that
// leaves the corresponding field empty, so service-wide constants (the proto
// import, its alias) are written once instead of on every target.
type yamlDefaults struct {
	ProtoImport string `yaml:"proto_import"`
	ProtoAlias  string `yaml:"proto_alias"`
}

// applyDefaults fills a target's derivable/shared fields: go_import from
// module+go_dir, out from go_dir, and proto_import/proto_alias from defaults.
// An explicit value on the target always wins.
func (c yamlConfig) applyTargetDefaults(t yamlTarget) yamlTarget {
	if t.GoImport == "" && c.Module != "" && t.GoDir != "" {
		t.GoImport = c.Module + "/" + t.GoDir
	}
	if t.Out == "" && t.GoDir != "" {
		t.Out = t.GoDir + "/mapper_generated.go"
	}
	if t.ProtoImport == "" {
		t.ProtoImport = c.Defaults.ProtoImport
	}
	if t.ProtoAlias == "" {
		t.ProtoAlias = c.Defaults.ProtoAlias
	}
	return t
}

// applyEnumDefaults fills an enum's shared proto_import/proto_alias from defaults
// and derives out from go_dir, mirroring applyTargetDefaults.
func (c yamlConfig) applyEnumDefaults(e yamlEnum) yamlEnum {
	if e.Out == "" && e.GoDir != "" {
		e.Out = e.GoDir + "/mapper_generated.go"
	}
	if e.ProtoImport == "" {
		e.ProtoImport = c.Defaults.ProtoImport
	}
	if e.ProtoAlias == "" {
		e.ProtoAlias = c.Defaults.ProtoAlias
	}
	return e
}

type yamlEnum struct {
	Group       string          `yaml:"group"`
	GoDir       string          `yaml:"go_dir"`
	Func        string          `yaml:"func"`
	Direction   string          `yaml:"direction"`
	ProtoImport string          `yaml:"proto_import"`
	ProtoAlias  string          `yaml:"proto_alias"`
	ProtoType   string          `yaml:"proto_type"`
	GoType      string          `yaml:"go_type"`
	Package     string          `yaml:"package"`
	Out         string          `yaml:"out"`
	Default     string          `yaml:"default"`
	Values      []yamlEnumValue `yaml:"values"`
}

type yamlEnumValue struct {
	Go    string `yaml:"go"`
	Proto string `yaml:"proto"`
}

type yamlTarget struct {
	Group          string                   `yaml:"group"`
	GoDir          string                   `yaml:"go_dir"`
	GoImport       string                   `yaml:"go_import"`
	GoAlias        string                   `yaml:"go_alias"`
	GoType         string                   `yaml:"go_type"`
	ProtoImport    string                   `yaml:"proto_import"`
	ProtoAlias     string                   `yaml:"proto_alias"`
	ProtoType      string                   `yaml:"proto_type"`
	Unwrap         string                   `yaml:"unwrap"`
	Direction      string                   `yaml:"direction"`
	FuncToProto    string                   `yaml:"func_to_proto"`
	FuncFromProto  string                   `yaml:"func_from_proto"`
	TargetPointer  *bool                    `yaml:"target_pointer"`
	SourcePointer  *bool                    `yaml:"source_pointer"`
	TimeFromProto  string                   `yaml:"time_from_proto"`
	Getters        bool                     `yaml:"getters"`
	WrapOutput     string                   `yaml:"wrap_output"`
	WrapField      string                   `yaml:"wrap_field"`
	Package        string                   `yaml:"package"`
	Out            string                   `yaml:"out"`
	Exclude        []string                 `yaml:"exclude"`
	Converters     map[string]yamlConverter `yaml:"converters"`
	FieldNames     string                   `yaml:"field_names"`
	FieldOverrides map[string]string        `yaml:"field_overrides"`
	ExtraArgs      []yamlExtraArg           `yaml:"extra_args"`
	Imports        []string                 `yaml:"imports"`
}

type yamlExtraArg struct {
	Name   string `yaml:"name"`
	Type   string `yaml:"type"`
	Field  string `yaml:"field"`
	Expr   string `yaml:"expr"`
	Before bool   `yaml:"before"`
}

type yamlConverter struct {
	ToProto    string `yaml:"to_proto"`
	FromProto  string `yaml:"from_proto"`
	WholeInput bool   `yaml:"whole_input"`
	Repeated   bool   `yaml:"repeated"`
	ElementPtr bool   `yaml:"element_ptr"`
	Deref      bool   `yaml:"deref"`
}

func loadConfig(path string) ([]targetConfig, []enumConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc yamlConfig
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse yaml: %w", err)
	}
	if len(doc.Targets) == 0 && len(doc.Enums) == 0 {
		return nil, nil, errors.New("config has no targets or enums")
	}

	targets := make([]targetConfig, 0, len(doc.Targets))
	for i, t := range doc.Targets {
		if t.Group != "" {
			g, ok := doc.Groups[t.Group]
			if !ok {
				return nil, nil, fmt.Errorf("targets[%d]: unknown group %q", i, t.Group)
			}
			t = g.merge(t)
		}
		resolved, err := resolveTarget(doc.applyTargetDefaults(t))
		if err != nil {
			return nil, nil, fmt.Errorf("targets[%d]: %w", i, err)
		}
		resolved.ConverterImport = doc.ConverterImport
		targets = append(targets, resolved)
	}

	enums := make([]enumConfig, 0, len(doc.Enums))
	for i, e := range doc.Enums {
		if e.Group != "" {
			g, ok := doc.Groups[e.Group]
			if !ok {
				return nil, nil, fmt.Errorf("enums[%d]: unknown group %q", i, e.Group)
			}
			e = g.mergeEnum(e)
		}
		resolved, err := resolveEnum(doc.applyEnumDefaults(e))
		if err != nil {
			return nil, nil, fmt.Errorf("enums[%d]: %w", i, err)
		}
		enums = append(enums, resolved)
	}
	return targets, enums, nil
}

func resolveEnum(e yamlEnum) (enumConfig, error) {
	required := []struct{ name, val string }{
		{"func", e.Func}, {"proto_import", e.ProtoImport}, {"proto_type", e.ProtoType},
		{"go_type", e.GoType}, {"package", e.Package}, {"out", e.Out},
	}
	for _, r := range required {
		if r.val == "" {
			return enumConfig{}, fmt.Errorf("%s is required", r.name)
		}
	}

	dir, err := parseDirection(e.Direction)
	if err != nil {
		return enumConfig{}, err
	}
	if dir == dirBoth {
		return enumConfig{}, errors.New("enum direction must be to_proto or from_proto")
	}
	if len(e.Values) == 0 {
		return enumConfig{}, errors.New("enum needs at least one value")
	}
	if dir == dirToProto && e.Default == "" {
		return enumConfig{}, errors.New("to_proto enum needs a default proto value")
	}
	if dir == dirFromProto && e.Default == "" && e.GoType != "string" {
		return enumConfig{}, errors.New("from_proto enum needs a default for a non-string go_type")
	}

	alias := e.ProtoAlias
	if alias == "" {
		alias = defaultProtoAlias(e.ProtoImport)
	}
	values := make([]enumValue, 0, len(e.Values))
	for _, v := range e.Values {
		if v.Go == "" || v.Proto == "" {
			return enumConfig{}, errors.New("enum value needs both go and proto")
		}
		values = append(values, enumValue{goExpr: v.Go, protoVal: v.Proto})
	}

	return enumConfig{
		Func: e.Func, Direction: dir, ProtoImport: e.ProtoImport, ProtoAlias: alias,
		ProtoType: e.ProtoType, GoType: e.GoType, Package: e.Package, Out: e.Out,
		Default: e.Default, Values: values,
	}, nil
}

// defaultedNames fills the optional alias, proto-type, and function-name fields
// from their conventional defaults when the config leaves them blank. Extracted
// from resolveTarget to keep that function under the complexity limit.
func defaultedNames(t yamlTarget) (goAlias, protoAlias, protoType, funcToProto, funcFromProto string) {
	goAlias = t.GoAlias
	if goAlias == "" {
		goAlias = defaultGoAlias(t.GoImport)
	}
	protoAlias = t.ProtoAlias
	if protoAlias == "" {
		protoAlias = defaultProtoAlias(t.ProtoImport)
	}
	protoType = t.ProtoType
	if protoType == "" {
		protoType = t.GoType
	}
	funcToProto = t.FuncToProto
	if funcToProto == "" {
		funcToProto = t.GoType + "ToProto"
	}
	funcFromProto = t.FuncFromProto
	if funcFromProto == "" {
		funcFromProto = t.GoType + "FromProto"
	}
	return goAlias, protoAlias, protoType, funcToProto, funcFromProto
}

// validateFromProtoOnly rejects knobs that only make sense when a from_proto
// function is emitted (i.e. direction is from_proto or both, not to_proto).
func validateFromProtoOnly(t yamlTarget, dir direction) error {
	if dir != dirToProto {
		return nil
	}
	switch {
	case t.Unwrap != "":
		return errors.New("unwrap is only valid with direction from_proto or both")
	case t.TimeFromProto != "":
		return errors.New("time_from_proto is only valid with direction from_proto or both")
	case t.Getters:
		return errors.New("getters is only valid with direction from_proto or both")
	}
	return nil
}

// resolveWrapOutput validates the wrap_output envelope knob and returns the
// effective envelope field name (defaulting to "Body"). Wrapping only makes
// sense for a from_proto-only mapping; it rejects to_proto/both, unwrap and
// extra_args. Repeated converters (list bodies) are allowed but require
// getters: true so the generated converter.Slice reads are nil-safe.
func resolveWrapOutput(t yamlTarget, dir direction, converters map[string]converterPair) (string, error) {
	if t.WrapOutput == "" {
		return "", nil
	}
	if dir != dirFromProto {
		return "", errors.New("wrap_output requires direction from_proto")
	}
	if t.Unwrap != "" {
		return "", errors.New("wrap_output cannot be combined with unwrap")
	}
	if len(t.ExtraArgs) > 0 {
		return "", errors.New("wrap_output cannot be combined with extra_args")
	}
	for name, c := range converters {
		if c.Repeated && !t.Getters {
			return "", fmt.Errorf("wrap_output repeated converter (%s) requires getters: true", name)
		}
	}
	field := t.WrapField
	if field == "" {
		field = "Body"
	}
	return field, nil
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
	if verr := validateFromProtoOnly(t, dir); verr != nil {
		return targetConfig{}, verr
	}

	goAlias, protoAlias, protoType, funcToProto, funcFromProto := defaultedNames(t)

	targetPointer := true
	if t.TargetPointer != nil {
		targetPointer = *t.TargetPointer
	}

	sourcePointer := true
	if t.SourcePointer != nil {
		sourcePointer = *t.SourcePointer
	}

	exclude := make(map[string]bool, len(t.Exclude))
	for _, name := range t.Exclude {
		exclude[name] = true
	}

	converters, err := resolveConverters(t.Converters, dir)
	if err != nil {
		return targetConfig{}, err
	}

	wrapField, err := resolveWrapOutput(t, dir, converters)
	if err != nil {
		return targetConfig{}, err
	}

	fieldNames, err := parseFieldNames(t.FieldNames)
	if err != nil {
		return targetConfig{}, err
	}

	extraArgs, err := resolveExtraArgs(t.ExtraArgs, dir)
	if err != nil {
		return targetConfig{}, err
	}

	imports := make([]importEntry, 0, len(t.Imports))
	for _, p := range t.Imports {
		imports = append(imports, importEntry{path: p})
	}

	// goSelf is true when the generated file lives in the same Go package
	// as the source struct. Detected by comparing the output directory to
	// the struct directory after cleaning both paths so a trailing slash or
	// "./" prefix doesn't matter. When true, the generator omits the go
	// import and emits bare type names (`CreateParams` instead of
	// `user.CreateParams`) to avoid an import cycle on itself.
	goSelf := filepath.Clean(filepath.Dir(t.Out)) == filepath.Clean(t.GoDir)

	return targetConfig{
		GoDir:          t.GoDir,
		GoImport:       t.GoImport,
		GoAlias:        goAlias,
		GoType:         t.GoType,
		GoSelf:         goSelf,
		ProtoImport:    t.ProtoImport,
		ProtoAlias:     protoAlias,
		ProtoType:      protoType,
		Unwrap:         t.Unwrap,
		Direction:      dir,
		FuncToProto:    funcToProto,
		FuncFromProto:  funcFromProto,
		TargetPointer:  targetPointer,
		SourcePointer:  sourcePointer,
		TimeFromProto:  t.TimeFromProto,
		Getters:        t.Getters,
		WrapOutput:     t.WrapOutput,
		WrapField:      wrapField,
		Package:        t.Package,
		Out:            t.Out,
		Exclude:        exclude,
		Converters:     converters,
		FieldNames:     fieldNames,
		FieldOverrides: t.FieldOverrides,
		ExtraArgs:      extraArgs,
		Imports:        imports,
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

// resolveExtraArgs validates and resolves the from_proto extra parameters.
// They only make sense on the from_proto side, where they inject signature
// parameters whose values populate target fields, so any other direction is an
// error.
func resolveExtraArgs(in []yamlExtraArg, dir direction) ([]extraArg, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if dir != dirFromProto {
		return nil, errors.New("extra_args is only valid with direction from_proto")
	}
	out := make([]extraArg, 0, len(in))
	for _, a := range in {
		if a.Name == "" || a.Type == "" || a.Field == "" {
			return nil, errors.New("extra_args entry needs name, type, and field")
		}
		expr := a.Expr
		if expr == "" {
			expr = a.Name
		}
		out = append(out, extraArg{
			name:   a.Name,
			typ:    a.Type,
			field:  a.Field,
			expr:   expr,
			before: a.Before,
		})
	}
	return out, nil
}

// importEntries returns the alias/path pairs a single member contributes
// to its enclosing file's import block. The Go-side import is skipped when
// the file is in the same package as the source struct (GoSelf). The
// timestamppb import is only needed when the to-proto direction emits a
// time.Time -> *timestamppb.Timestamp conversion that has no override.
func importEntries(m genMember) []importEntry {
	if m.enum != nil {
		// An enum member only references its proto enum type.
		return []importEntry{{alias: m.enum.ProtoAlias, path: m.enum.ProtoImport}}
	}
	out := make([]importEntry, 0, 3)
	if !m.cfg.GoSelf {
		out = append(out, importEntry{alias: m.cfg.GoAlias, path: m.cfg.GoImport})
	}
	out = append(out, importEntry{alias: m.cfg.ProtoAlias, path: m.cfg.ProtoImport})
	if needsTimestamppb(m.fields, m.cfg) {
		out = append(out, importEntry{alias: "", path: timestamppbImport})
	}
	if m.cfg.ConverterImport != "" && m.cfg.usesConverter() {
		out = append(out, importEntry{alias: "", path: m.cfg.ConverterImport})
	}
	out = append(out, m.cfg.Imports...)
	return out
}

// kind enumerates the classes of Go field types mapgen proto recognizes.
type kind int

const (
	kindUnknown kind = iota
	kindScalar
	kindPtrScalar
	kindByteSlice
	kindScalarSlice // []string, []int32, ... <-> proto repeated scalar (same Go type)
	kindRelation
	kindTime // time.Time <-> *timestamppb.Timestamp
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
		if codegen.StructTag(f).Get("bun") != "" {
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

		tag := codegen.StructTag(f)
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
		if ident, ok := t.Elt.(*ast.Ident); ok {
			if ident.Name == "byte" {
				return kindByteSlice, "[]byte"
			}
			// []scalar matches a proto repeated scalar field 1:1 in Go, so it
			// assigns directly (no per-element converter needed).
			if scalarTypes[ident.Name] {
				return kindScalarSlice, "[]" + ident.Name
			}
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

// renderGroup writes the merged-file source for one group of members
// sharing an output path. The header is followed by a single deduplicated
// import block, then each member's mapper functions in YAML order.
func renderGroup(members []genMember) ([]byte, error) {
	if len(members) == 0 {
		return nil, errors.New("renderGroup called with no members")
	}

	var buf bytes.Buffer
	buf.WriteString("// Code generated by mapgen proto; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", memberPackage(members[0]))

	// Dedup imports by path while preserving first-seen alias and ordering.
	seenPath := map[string]bool{}
	var imports []importEntry
	for _, m := range members {
		for _, e := range importEntries(m) {
			if seenPath[e.path] {
				continue
			}
			seenPath[e.path] = true
			imports = append(imports, e)
		}
	}
	buf.WriteString("import (\n")
	for _, e := range imports {
		if e.alias == "" {
			fmt.Fprintf(&buf, "\t%q\n", e.path)
		} else {
			fmt.Fprintf(&buf, "\t%s %q\n", e.alias, e.path)
		}
	}
	buf.WriteString(")\n\n")

	for i, m := range members {
		if i > 0 {
			buf.WriteString("\n")
		}
		renderMember(&buf, m)
	}

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated source: %w\n--- source ---\n%s", err, buf.String())
	}
	return out, nil
}

// renderMember writes one member's declarations: an enum switch, or the
// to_proto/from_proto mapper functions selected by the target's direction.
func renderMember(buf *bytes.Buffer, m genMember) {
	if m.enum != nil {
		writeEnum(buf, *m.enum)
		return
	}
	if m.cfg.Direction == dirBoth || m.cfg.Direction == dirToProto {
		writeToProto(buf, m.cfg, m.fields)
		if m.cfg.Direction == dirBoth {
			buf.WriteString("\n")
		}
	}
	if m.cfg.Direction == dirBoth || m.cfg.Direction == dirFromProto {
		writeFromProto(buf, m.cfg, m.fields)
	}
}

// generate is the single-target convenience used by tests. It delegates to
// renderGroup with a one-member group so both code paths produce identical
// output for the common case.
func generate(cfg targetConfig, fields []field, isBun bool) ([]byte, error) {
	return renderGroup([]genMember{{cfg: cfg, fields: fields, isBun: isBun}})
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
	return goRefName(cfg, cfg.GoType)
}

func writeToProto(buf *bytes.Buffer, cfg targetConfig, fields []field) {
	srcType := goTypeRef(cfg)
	dstType := fmt.Sprintf("%s.%s", cfg.ProtoAlias, outerProtoType(cfg))

	fmt.Fprintf(buf, "// %s maps a %s to its proto representation.\n", cfg.FuncToProto, cfg.GoType)
	buf.WriteString("// Generated by mapgen proto - hand edits will be overwritten.\n")
	if cfg.SourcePointer {
		fmt.Fprintf(buf, "func %s(src *%s) *%s {\n", cfg.FuncToProto, srcType, dstType)
		buf.WriteString("\tif src == nil {\n\t\treturn nil\n\t}\n")
	} else {
		fmt.Fprintf(buf, "func %s(src %s) *%s {\n", cfg.FuncToProto, srcType, dstType)
	}
	fmt.Fprintf(buf, "\tdst := &%s{\n", dstType)

	var repeated []field
	for _, f := range fields {
		if cfg.Exclude[f.goName] {
			continue
		}
		if conv, ok := cfg.Converters[f.goName]; ok && conv.ToProto != "" {
			if conv.Repeated {
				repeated = append(repeated, f)
				continue
			}
			fmt.Fprintf(buf, "\t\t%s: %s(src.%s),\n", protoFieldRef(cfg, f), conv.ToProto, f.goName)
			continue
		}
		switch {
		case isDirectMappable(f.kind):
			fmt.Fprintf(buf, "\t\t%s: src.%s,\n", protoFieldRef(cfg, f), f.goName)
		case f.kind == kindTime:
			fmt.Fprintf(buf, "\t\t%s: timestamppb.New(src.%s),\n", protoFieldRef(cfg, f), f.goName)
		default:
			fmt.Fprintf(buf, "\t\t// TODO: %s (%s)\n", f.goName, todoSuffix(f))
		}
	}

	buf.WriteString("\t}\n")
	for _, f := range repeated {
		writeRepeatedToProto(buf, cfg, f, cfg.Converters[f.goName])
	}
	buf.WriteString("\treturn dst\n")
	buf.WriteString("}\n")
}

// goRefName qualifies a Go type name from the source struct's package: bare
// when the generated file is in that package (GoSelf), else alias-qualified.
func goRefName(cfg targetConfig, name string) string {
	if cfg.GoSelf {
		return name
	}
	return cfg.GoAlias + "." + name
}

// writeFromProtoWrapped emits a from_proto function that returns a huma-style
// envelope: out := &Output{}; nil input yields the empty envelope; otherwise the
// mapped body is assigned to out.<WrapField>. Repeated/extra-arg/unwrap configs
// are rejected upstream, so the body is a single flat struct literal.
func writeFromProtoWrapped(buf *bytes.Buffer, cfg targetConfig, fields []field) {
	outerType := fmt.Sprintf("%s.%s", cfg.ProtoAlias, outerProtoType(cfg))
	bodyType := goTypeRef(cfg)
	outputType := goRefName(cfg, cfg.WrapOutput)

	fmt.Fprintf(buf, "// %s maps a proto message into a %s envelope.\n", cfg.FuncFromProto, cfg.WrapOutput)
	buf.WriteString("// Generated by mapgen proto - hand edits will be overwritten.\n")
	fmt.Fprintf(buf, "func %s(in *%s) *%s {\n", cfg.FuncFromProto, outerType, outputType)
	fmt.Fprintf(buf, "\tout := &%s{}\n", outputType)
	// With getters every read is nil-safe (in.GetX() handles a nil receiver and
	// SliceDeref/Slice return [] for a nil slice), so no nil guard is needed and
	// empty list bodies stay []. Without getters, keep the guard + direct reads.
	if !cfg.Getters {
		if zero := fromProtoZeroValue(cfg, bodyType, fields); zero != bodyType+"{}" {
			fmt.Fprintf(buf, "\tout.%s = %s\n", cfg.WrapField, zero)
		}
		buf.WriteString("\tif in == nil {\n\t\treturn out\n\t}\n")
	}
	fmt.Fprintf(buf, "\tout.%s = %s{\n", cfg.WrapField, bodyType)
	for _, f := range fields {
		if cfg.Exclude[f.goName] {
			continue
		}
		if expr := wrappedFieldExpr(cfg, f); expr != "" {
			fmt.Fprintf(buf, "\t\t%s: %s,\n", f.goName, expr)
		} else {
			fmt.Fprintf(buf, "\t\t// TODO: %s (%s)\n", f.goName, todoSuffix(f))
		}
	}
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn out\n")
	buf.WriteString("}\n")
}

// wrappedFieldExpr builds the value expression for one body field of a
// wrap_output target: a converter call, a repeated converter.Slice/SliceDeref,
// a time helper, or a direct read. All reads go through protoFieldRead so they
// honor cfg.Getters.
func wrappedFieldExpr(cfg targetConfig, f field) string {
	read := protoFieldRead(cfg, "in", f)
	if conv, ok := cfg.Converters[f.goName]; ok && conv.FromProto != "" {
		switch {
		case conv.Repeated && conv.Deref:
			return fmt.Sprintf("converter.SliceDeref(%s, %s)", read, conv.FromProto)
		case conv.Repeated:
			return fmt.Sprintf("converter.Slice(%s, %s)", read, conv.FromProto)
		case conv.WholeInput:
			return fmt.Sprintf("%s(in)", conv.FromProto)
		default:
			return fmt.Sprintf("%s(%s)", conv.FromProto, read)
		}
	}
	switch {
	case f.kind == kindTime && cfg.TimeFromProto != "":
		return fmt.Sprintf("%s(%s)", cfg.TimeFromProto, read)
	case f.kind == kindTime:
		return read + ".AsTime()"
	case f.kind == kindScalarSlice:
		return nonNilSliceExpr(f.rawType, read)
	case isDirectMappable(f.kind):
		return read
	default:
		return ""
	}
}

func writeFromProto(buf *bytes.Buffer, cfg targetConfig, fields []field) {
	if cfg.WrapOutput != "" {
		writeFromProtoWrapped(buf, cfg, fields)
		return
	}
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
		returnZero = fromProtoZeroValue(cfg, goType, fields)
		openLiteral = goType + "{"
	}

	fmt.Fprintf(buf, "// %s maps a proto message back to %s.\n", cfg.FuncFromProto, cfg.GoType)
	buf.WriteString("// Generated by mapgen proto - hand edits will be overwritten.\n")
	fmt.Fprintf(buf, "func %s(%s) %s {\n", cfg.FuncFromProto, fromProtoParams(cfg, outerType), returnType)
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

	argByField := make(map[string]extraArg, len(cfg.ExtraArgs))
	for _, a := range cfg.ExtraArgs {
		argByField[a.field] = a
	}

	var repeated []field
	for _, f := range fields {
		if fromProtoField(buf, cfg, f, readVar, argByField) {
			repeated = append(repeated, f)
		}
	}

	buf.WriteString("\t}\n")
	for _, f := range repeated {
		writeRepeatedFromProto(buf, cfg, f, readVar, cfg.Converters[f.goName])
	}
	buf.WriteString("\treturn dst\n")
	buf.WriteString("}\n")
}

// fromProtoField emits the struct-literal entry for one field on the from_proto
// side and reports whether the field is a repeated converter, which the caller
// defers to a post-literal append loop. Excluded fields and extra-arg/converter
// targets are handled here; everything else falls back to direct, time, or TODO.
func fromProtoField(buf *bytes.Buffer, cfg targetConfig, f field, readVar string, argByField map[string]extraArg) (deferred bool) {
	if cfg.Exclude[f.goName] {
		return false
	}
	if a, ok := argByField[f.goName]; ok {
		// Field is supplied by a caller argument, not read from the proto.
		fmt.Fprintf(buf, "\t\t%s: %s,\n", f.goName, a.expr)
		return false
	}
	if conv, ok := cfg.Converters[f.goName]; ok && conv.FromProto != "" {
		switch {
		case conv.Repeated:
			return true
		case conv.WholeInput:
			fmt.Fprintf(buf, "\t\t%s: %s(%s),\n", f.goName, conv.FromProto, readVar)
		default:
			fmt.Fprintf(buf, "\t\t%s: %s(%s.%s),\n", f.goName, conv.FromProto, readVar, protoFieldRef(cfg, f))
		}
		return false
	}
	switch {
	case f.kind == kindScalarSlice:
		fmt.Fprintf(buf, "\t\t%s: %s,\n", f.goName, nonNilSliceExpr(f.rawType, protoFieldRead(cfg, readVar, f)))
	case isDirectMappable(f.kind):
		fmt.Fprintf(buf, "\t\t%s: %s,\n", f.goName, protoFieldRead(cfg, readVar, f))
	case f.kind == kindTime && cfg.TimeFromProto != "":
		// A helper (e.g. protoutil.TimeFromProto) controls the unset-timestamp
		// value; it receives the raw *timestamppb.Timestamp field.
		fmt.Fprintf(buf, "\t\t%s: %s(%s.%s),\n", f.goName, cfg.TimeFromProto, readVar, protoFieldRef(cfg, f))
	case f.kind == kindTime:
		// AsTime is nil-safe - returns the zero epoch when unset.
		fmt.Fprintf(buf, "\t\t%s: %s.%s.AsTime(),\n", f.goName, readVar, protoFieldRef(cfg, f))
	default:
		fmt.Fprintf(buf, "\t\t// TODO: %s (%s)\n", f.goName, todoSuffix(f))
	}
	return false
}

func fromProtoZeroValue(cfg targetConfig, goType string, fields []field) string {
	var entries []field
	for _, f := range fields {
		if fromProtoNeedsNonNilSliceInit(cfg, f) {
			entries = append(entries, f)
		}
	}
	if len(entries) == 0 {
		return goType + "{}"
	}

	var b strings.Builder
	b.WriteString(goType)
	b.WriteString("{\n")
	for _, f := range entries {
		fmt.Fprintf(&b, "\t\t\t%s: %s{},\n", f.goName, f.rawType)
	}
	b.WriteString("\t\t}")
	return b.String()
}

func fromProtoNeedsNonNilSliceInit(cfg targetConfig, f field) bool {
	if cfg.Exclude[f.goName] {
		return false
	}
	if conv, ok := cfg.Converters[f.goName]; ok && conv.FromProto != "" {
		return conv.Repeated && strings.HasPrefix(f.rawType, "[]")
	}
	return f.kind == kindScalarSlice
}

func nonNilSliceExpr(sliceType, read string) string {
	return fmt.Sprintf("append(make(%s, 0, len(%s)), %s...)", sliceType, read, read)
}

// writeRepeatedToProto emits a post-literal assignment mapping a Go slice field
// to the proto repeated field via converter.Slice (element mapper takes the
// element as-is) or converter.SlicePtr (element_ptr: true → mapper takes *elem).
// Both yield a non-nil empty slice for an empty/nil source, matching the
// hand-written []-init. Targets using this need an `imports:` entry for
// lib-util/v3/converter.
func writeRepeatedToProto(buf *bytes.Buffer, cfg targetConfig, f field, conv converterPair) {
	fn := "converter.Slice"
	if conv.ElementPtr {
		fn = "converter.SlicePtr"
	}
	fmt.Fprintf(buf, "\tdst.%s = %s(src.%s, %s)\n", protoFieldRef(cfg, f), fn, f.goName, conv.ToProto)
}

// writeRepeatedFromProto emits a post-literal assignment that maps a proto
// repeated field via converter.Slice (element mapper returns a value) or
// converter.SliceDeref (returns *D, nil elements skipped). Both return a
// non-nil empty slice for an empty/nil source, so the Go field stays [] rather
// than nil — preserving the hand-written []-init semantics the frontend expects.
// Targets using this need an `imports:` entry for lib-util/v3/converter.
func writeRepeatedFromProto(buf *bytes.Buffer, cfg targetConfig, f field, readVar string, conv converterPair) {
	fn := "converter.Slice"
	if conv.Deref {
		fn = "converter.SliceDeref"
	}
	fmt.Fprintf(buf, "\tdst.%s = %s(%s.%s, %s)\n", f.goName, fn, readVar, protoFieldRef(cfg, f), conv.FromProto)
}

// enumGoExpr renders the Go side of one enum value: a quoted literal when the
// non-proto type is string, otherwise the bare constant identifier.
func enumGoExpr(e enumConfig, raw string) string {
	if e.GoType == "string" {
		return fmt.Sprintf("%q", raw)
	}
	return raw
}

// writeEnum emits a one-direction switch function mapping a Go value to/from a
// proto enum. Proto enum constants are spelled <alias>.<ProtoType>_<value>.
func writeEnum(buf *bytes.Buffer, e enumConfig) {
	protoType := fmt.Sprintf("%s.%s", e.ProtoAlias, e.ProtoType)
	constOf := func(suffix string) string {
		return fmt.Sprintf("%s.%s_%s", e.ProtoAlias, e.ProtoType, suffix)
	}

	fmt.Fprintf(buf, "// %s is generated by mapgen proto - hand edits will be overwritten.\n", e.Func)
	if e.Direction == dirToProto {
		fmt.Fprintf(buf, "func %s(v %s) %s {\n", e.Func, e.GoType, protoType)
		buf.WriteString("\tswitch v {\n")
		for _, val := range e.Values {
			fmt.Fprintf(buf, "\tcase %s:\n\t\treturn %s\n", enumGoExpr(e, val.goExpr), constOf(val.protoVal))
		}
		fmt.Fprintf(buf, "\tdefault:\n\t\treturn %s\n", constOf(e.Default))
		buf.WriteString("\t}\n}\n")
		return
	}

	fmt.Fprintf(buf, "func %s(v %s) %s {\n", e.Func, protoType, e.GoType)
	buf.WriteString("\tswitch v {\n")
	for _, val := range e.Values {
		fmt.Fprintf(buf, "\tcase %s:\n\t\treturn %s\n", constOf(val.protoVal), enumGoExpr(e, val.goExpr))
	}
	fmt.Fprintf(buf, "\tdefault:\n\t\treturn %s\n", enumGoExpr(e, e.Default))
	buf.WriteString("\t}\n}\n")
}

// fromProtoParams builds the parameter list for a from_proto function: any
// extra args flagged `before` precede the proto input, the rest follow it, each
// group preserving config order.
func fromProtoParams(cfg targetConfig, outerType string) string {
	var before, after []string
	for _, a := range cfg.ExtraArgs {
		decl := a.name + " " + a.typ
		if a.before {
			before = append(before, decl)
		} else {
			after = append(after, decl)
		}
	}
	parts := make([]string, 0, len(cfg.ExtraArgs)+1)
	parts = append(parts, before...)
	parts = append(parts, "in *"+outerType)
	parts = append(parts, after...)
	return strings.Join(parts, ", ")
}

// protoFieldRef returns the Go identifier of the proto field corresponding to
// a Go struct field. An explicit field_overrides entry always wins. Bun-mode
// fields carry a column tag whose snake_case becomes PascalCase. Plain-mode
// fields use the Go field name verbatim by default (field_names: identity) or,
// with field_names: protoc, run it through protoc-gen-go's algorithm so
// initialism fields such as SkillID line up with proto's SkillId.
func protoFieldRef(cfg targetConfig, f field) string {
	if override, ok := cfg.FieldOverrides[f.goName]; ok {
		return override
	}
	if f.column != "" {
		return codegen.ProtoGoName(f.column)
	}
	if cfg.FieldNames == namesProtoc {
		return codegen.ProtoGoName(codegen.Snake(f.goName))
	}
	return f.goName
}

func isDirectMappable(k kind) bool {
	return k == kindScalar || k == kindPtrScalar || k == kindByteSlice || k == kindScalarSlice
}

// protoFieldRead returns the expression that reads a directly-mappable proto
// field on the from_proto side. With cfg.Getters, value-typed fields use the
// generated accessor (in.GetX()), which dereferences a proto3-optional scalar to
// its value type so it maps onto a non-pointer Go field; pointer-scalar targets
// keep direct field access (in.X) so the pointer is preserved.
func protoFieldRead(cfg targetConfig, readVar string, f field) string {
	if cfg.Getters && f.kind != kindPtrScalar {
		return fmt.Sprintf("%s.Get%s()", readVar, protoFieldRef(cfg, f))
	}
	return fmt.Sprintf("%s.%s", readVar, protoFieldRef(cfg, f))
}

func todoSuffix(f field) string {
	if f.kind == kindRelation {
		return f.rawType + ", relation"
	}
	return f.rawType
}
