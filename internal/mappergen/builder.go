package mappergen

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/kitti12911/lib-orm/v4/internal/codegen"
)

// builder holds the resolved proto side and target registries for one feature.
type builder struct {
	g         *generator
	pkg       string
	params    map[string]goStruct
	messages  map[string]goStruct
	enums     map[string]protoEnum
	toProto   map[string]string // proto message name -> to_proto func name
	fromProto map[string]string // proto message name -> from_proto func name
	usedEnums map[string]bool
	needsTime bool
}

func (b *builder) paramFields(name string) []goField { return b.params[name].fields }

// target is one mapper to emit.
type target struct {
	goType    string // source Go struct (bun model or params)
	protoType string // proto message name
	valueRet  bool   // from_proto: return by value (root params) vs pointer
}

// discoverToTargets returns to_proto targets: the root model plus every relation
// model reachable from it that has a matching proto message, in BFS order.
func (b *builder) discoverToTargets(root string) []target {
	var out []target
	for _, name := range b.reachableModels(root) {
		if _, ok := b.messages[name]; ok {
			out = append(out, target{goType: name, protoType: name})
		}
	}
	return out
}

func (b *builder) reachableModels(root string) []string {
	seen := map[string]bool{root: true}
	order := []string{root}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		rels := append([]string(nil), b.g.relations[cur]...)
		sort.Strings(rels)
		for _, r := range rels {
			if seen[r] {
				continue
			}
			if _, ok := b.g.models[r]; !ok {
				continue
			}
			seen[r] = true
			order = append(order, r)
			queue = append(queue, r)
		}
	}
	return order
}

// discoverFromTargets returns from_proto targets for every Create<X>Params struct,
// ordered root-first then by nesting. protoType is //mapgen:proto override or the
// derived <root><X>.
func (b *builder) discoverFromTargets(params map[string]goStruct, root string) []target {
	byGoType, rootParams := b.collectParamTargets(params, root)
	if rootParams == "" {
		out := make([]target, 0, len(byGoType))
		for _, n := range sortedKeys(byGoType) {
			out = append(out, byGoType[n])
		}
		return out
	}
	return orderFromTargets(byGoType, rootParams, params)
}

// collectParamTargets finds every Create<X>Params with a matching proto message.
func (b *builder) collectParamTargets(params map[string]goStruct, root string) (byGoType map[string]target, rootParams string) {
	byGoType = map[string]target{}
	for name, s := range params {
		if !strings.HasPrefix(name, "Create") || !strings.HasSuffix(name, "Params") {
			continue
		}
		protoType := s.protoOverride
		if protoType == "" {
			protoType = root + strings.TrimSuffix(strings.TrimPrefix(name, "Create"), "Params")
		}
		if _, ok := b.messages[protoType]; !ok {
			continue
		}
		byGoType[name] = target{goType: name, protoType: protoType, valueRet: protoType == root}
		if protoType == root {
			rootParams = name
		}
	}
	return byGoType, rootParams
}

// orderFromTargets returns targets in BFS order from the root params over nested
// Create*Params fields, appending any unreachable ones alphabetically.
func orderFromTargets(byGoType map[string]target, rootParams string, params map[string]goStruct) []target {
	var out []target
	seen := map[string]bool{}
	queue := []string{rootParams}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if t, ok := byGoType[cur]; ok {
			out = append(out, t)
		}
		for _, f := range params[cur].fields {
			if _, ok := byGoType[f.base]; ok && !seen[f.base] {
				queue = append(queue, f.base)
			}
		}
	}
	for _, n := range sortedKeys(byGoType) {
		if !seen[n] {
			out = append(out, byGoType[n])
		}
	}
	return out
}

// ctorTarget is one params->model constructor to emit.
type ctorTarget struct {
	paramsType string    // e.g. "CreateProfileParams"
	modelType  string    // e.g. "UserProfile"
	fkArgs     []goField // model FK fields promoted to leading args, declared order
}

// discoverCtorTargets returns a constructor target for every Create<X>Params
// whose derived model <root><X> exists, root params first then alphabetical.
func (b *builder) discoverCtorTargets(params map[string]goStruct, root string) []ctorTarget {
	var rootT *ctorTarget
	var rest []ctorTarget
	for _, name := range sortedKeys(params) {
		if !strings.HasPrefix(name, "Create") || !strings.HasSuffix(name, "Params") {
			continue
		}
		model := root + strings.TrimSuffix(strings.TrimPrefix(name, "Create"), "Params")
		ms, ok := b.g.models[model]
		if !ok {
			continue
		}
		t := ctorTarget{paramsType: name, modelType: model, fkArgs: b.ctorFKArgs(ms, params[name])}
		if model == root {
			rootT = &t
			continue
		}
		rest = append(rest, t)
	}
	if rootT != nil {
		return append([]ctorTarget{*rootT}, rest...)
	}
	return rest
}

// ctorFKArgs returns the model's FK fields that become leading constructor args:
// non-pointer, name ends in "ID" (but is not the pk "ID"), not a relation, and
// absent from the params struct.
func (b *builder) ctorFKArgs(model, params goStruct) []goField {
	paramNames := map[string]bool{}
	for _, f := range params.fields {
		paramNames[f.name] = true
	}
	var out []goField
	for _, f := range model.fields {
		if f.name == "ID" || !strings.HasSuffix(f.name, "ID") || f.ptr || paramNames[f.name] {
			continue
		}
		if _, isModel := b.g.models[f.base]; isModel {
			continue
		}
		out = append(out, f)
	}
	return out
}

// renderCtor emits <lowerModel>ModelFrom<Params>(fkArgs..., params <Params>) *database.<Model>.
func (b *builder) renderCtor(buf *bytes.Buffer, t ctorTarget) {
	fn := lowerFirst(t.modelType) + "ModelFrom" + t.paramsType
	args := make([]string, 0, len(t.fkArgs)+1)
	for _, fk := range t.fkArgs {
		args = append(args, lowerFirst(fk.name)+" "+fk.typ)
	}
	args = append(args, "params "+t.paramsType)
	fmt.Fprintf(buf, "// %s builds a %s.%s from %s.\n", fn, dbAlias, t.modelType, t.paramsType)
	fmt.Fprintf(buf, "func %s(%s) *%s.%s {\n", fn, strings.Join(args, ", "), dbAlias, t.modelType)
	fmt.Fprintf(buf, "\treturn &%s.%s{\n", dbAlias, t.modelType)

	paramFields := fieldSet(b.params[t.paramsType])
	for _, mf := range b.g.models[t.modelType].fields {
		if isFK(t.fkArgs, mf.name) {
			fmt.Fprintf(buf, "\t\t%s: %s,\n", mf.name, lowerFirst(mf.name))
			continue
		}
		if pf, ok := paramFields[mf.name]; ok && pf.typ == mf.typ {
			fmt.Fprintf(buf, "\t\t%s: params.%s,\n", mf.name, mf.name)
		}
	}
	buf.WriteString("\t}\n}\n\n")
}

func isFK(fks []goField, name string) bool {
	for _, f := range fks {
		if f.name == name {
			return true
		}
	}
	return false
}

// renderToProto emits toProto<Model>(src *database.<Model>) *<pb>.<Model>.
func (b *builder) renderToProto(buf *bytes.Buffer, t target) {
	fn := b.toProto[t.protoType]
	pb := b.protoAlias()
	fmt.Fprintf(buf, "// %s maps a %s to its proto representation.\n", fn, t.goType)
	fmt.Fprintf(buf, "func %s(src *%s.%s) *%s.%s {\n", fn, dbAlias, t.goType, pb, t.protoType)
	buf.WriteString("\tif src == nil {\n\t\treturn nil\n\t}\n")
	fmt.Fprintf(buf, "\tdst := &%s.%s{\n", pb, t.protoType)

	msg := b.messages[t.protoType]
	protoFields := fieldSet(msg)
	assigns := map[string]string{}
	for _, f := range b.g.models[t.goType].fields {
		if strings.HasPrefix(f.typ, "[]") {
			continue // slices are handled by envelope/list mappers only
		}
		protoName := codegen.ProtoGoName(codegen.Snake(f.name))
		pf, ok := protoFields[protoName]
		if !ok {
			continue // no proto counterpart -> excluded by intersection
		}
		assigns[protoName] = b.toProtoExpr(f, pf, "src")
	}
	for _, name := range sortedKeys(assigns) {
		fmt.Fprintf(buf, "\t\t%s: %s,\n", name, assigns[name])
	}
	buf.WriteString("\t}\n\treturn dst\n}\n\n")
}

// renderFromProto emits <lowerParams>FromProto(in *<pb>.<Msg>) <Params>.
func (b *builder) renderFromProto(buf *bytes.Buffer, t target) {
	fn := b.fromProto[t.protoType]
	pb := b.protoAlias()
	ret := t.goType
	empty := t.goType + "{}"
	if !t.valueRet {
		ret = "*" + t.goType
		empty = "nil"
	}
	fmt.Fprintf(buf, "// %s maps %s.%s back to %s.\n", fn, pb, t.protoType, t.goType)
	fmt.Fprintf(buf, "func %s(in *%s.%s) %s {\n", fn, pb, t.protoType, ret)
	fmt.Fprintf(buf, "\tif in == nil {\n\t\treturn %s\n\t}\n", empty)
	if t.valueRet {
		fmt.Fprintf(buf, "\tdst := %s{\n", t.goType)
	} else {
		fmt.Fprintf(buf, "\tdst := &%s{\n", t.goType)
	}

	msg := b.messages[t.protoType]
	protoFields := fieldSet(msg)
	assigns := map[string]string{}
	for _, f := range b.paramFields(t.goType) {
		if strings.HasPrefix(f.typ, "[]") {
			continue // slices are handled by envelope/list mappers only
		}
		protoName := codegen.ProtoGoName(codegen.Snake(f.name))
		pf, ok := protoFields[protoName]
		if !ok {
			continue
		}
		assigns[f.name] = b.fromProtoExpr(pf, "in."+protoName)
	}
	for _, name := range sortedKeys(assigns) {
		fmt.Fprintf(buf, "\t\t%s: %s,\n", name, assigns[name])
	}
	buf.WriteString("\t}\n\treturn dst\n}\n\n")
}

// toProtoExpr builds the RHS assigning a source field to a proto field.
func (b *builder) toProtoExpr(src, pf goField, recv string) string {
	access := recv + "." + src.name
	switch {
	case pf.typ == timestampType && src.base == "time.Time" && !src.ptr:
		b.needsTime = true
		return "timestamppb.New(" + access + ")"
	case b.isEnum(pf.base):
		b.usedEnums[pf.base] = true
		return "toProto" + pf.base + "(" + access + ")"
	case b.isMessage(pf.base) && pf.ptr:
		return b.toProto[pf.base] + "(" + access + ")"
	default:
		return access
	}
}

// fromProtoExpr builds the RHS reading a proto field into a target field.
func (b *builder) fromProtoExpr(pf goField, access string) string {
	switch {
	case b.isEnum(pf.base):
		b.usedEnums[pf.base] = true
		return lowerFirst(pf.base) + "FromProto(" + access + ")"
	case b.isMessage(pf.base) && pf.ptr:
		return b.fromProto[pf.base] + "(" + access + ")"
	default:
		return access
	}
}

// renderEnumBridges emits string<->enum switch functions for every enum used.
func (b *builder) renderEnumBridges(buf *bytes.Buffer) {
	pb := b.protoAlias()
	for _, name := range sortedKeys(boolSet(b.usedEnums)) {
		e := b.enums[name]

		fmt.Fprintf(buf, "// toProto%s maps a string to the %s.%s enum.\n", name, pb, name)
		fmt.Fprintf(buf, "func toProto%s(value string) %s.%s {\n\tswitch value {\n", name, pb, name)
		for _, v := range e.values {
			fmt.Fprintf(buf, "\tcase %q:\n\t\treturn %s.%s\n", v.str, pb, v.constName)
		}
		fmt.Fprintf(buf, "\tdefault:\n\t\treturn %s.%s\n\t}\n}\n\n", pb, unspecifiedConst(name))

		fmt.Fprintf(buf, "// %sFromProto maps the %s.%s enum to a string.\n", lowerFirst(name), pb, name)
		fmt.Fprintf(buf, "func %sFromProto(value %s.%s) string {\n\tswitch value {\n", lowerFirst(name), pb, name)
		for _, v := range e.values {
			fmt.Fprintf(buf, "\tcase %s.%s:\n\t\treturn %q\n", pb, v.constName, v.str)
		}
		buf.WriteString("\tdefault:\n\t\treturn \"\"\n\t}\n}\n\n")
	}
}

// assemble wraps the body with the header and computed import block.
func (b *builder) assemble(body []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by mapgen map; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", b.feature())

	buf.WriteString("import (\n")
	if b.needsTime {
		fmt.Fprintf(&buf, "\t%q\n", timestampPkg)
	}
	fmt.Fprintf(&buf, "\t%s %q\n", b.protoAlias(), b.protoImport())
	fmt.Fprintf(&buf, "\t%s %q\n", dbAlias, b.g.module+"/"+modelDirRel)
	buf.WriteString(")\n\n")

	buf.Write(body)
	return buf.Bytes()
}

const dbAlias = "database"

func (b *builder) feature() string    { return b.pkg }
func (b *builder) protoAlias() string { return b.g.feature + "v1" }
func (b *builder) protoImport() string {
	return b.g.module + "/" + protoDirRel + "/" + b.g.feature + "/v1"
}
func (b *builder) isEnum(n string) bool { _, ok := b.enums[n]; return ok }

func (b *builder) isMessage(n string) bool { _, ok := b.messages[n]; return ok }

func fieldSet(s goStruct) map[string]goField {
	m := make(map[string]goField, len(s.fields))
	for _, f := range s.fields {
		m[f.name] = f
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func boolSet(m map[string]bool) map[string]struct{} {
	out := make(map[string]struct{}, len(m))
	for k, v := range m {
		if v {
			out[k] = struct{}{}
		}
	}
	return out
}

func unspecifiedConst(enum string) string {
	return enum + "_" + strings.ToUpper(codegen.Snake(enum)) + "_UNSPECIFIED"
}
