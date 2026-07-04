package mappergen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/kitti12911/lib-orm/v4/internal/codegen"
)

const (
	apiDirRel     = "internal/api"
	protoutilPkg  = "github.com/kitti12911/lib-util/v3/protoutil"
	timeType      = "time.Time"
	createPrefix  = "Create"
	requestSuffix = "Request"
)

// runOAS generates huma<->proto model mappers for every internal/api/<domain>/v1
// package (a huma REST gateway). Envelope, list, and query-input mappers are out
// of scope for this pass and stay hand-written (see plan/oas-mapper-generator.md).
func runOAS(dir, module string) error {
	apiRoot := filepath.Join(dir, apiDirRel)
	return walkVersionDirs(apiRoot, func(domainDir string) error {
		return generateOASDomain(dir, domainDir, module)
	})
}

// walkVersionDirs invokes fn for each internal/api/<domain>/v1 directory.
func walkVersionDirs(apiRoot string, fn func(domainDir string) error) error {
	domains, err := os.ReadDir(apiRoot)
	if err != nil {
		return fmt.Errorf("read api directory: %w", err)
	}
	for _, d := range domains {
		if !d.IsDir() {
			continue
		}
		versionDir := filepath.Join(apiRoot, d.Name(), "v1")
		if _, err := os.Stat(versionDir); err != nil {
			continue
		}
		if err := fn(versionDir); err != nil {
			return fmt.Errorf("api %s: %w", d.Name(), err)
		}
	}
	return nil
}

func generateOASDomain(repoRoot, domainDir, module string) error {
	pkg, humas, ignore, err := parseFeatureStructs(domainDir)
	if err != nil {
		return err
	}
	if ignore {
		return nil
	}

	protoDir, alias, protoImport, err := findProtoImport(repoRoot, domainDir, module)
	if err != nil || protoDir == "" {
		return nil //nolint:nilerr // no proto import -> nothing to map
	}
	messages, enums, err := parseProtoPackage(protoDir)
	if err != nil {
		return err
	}

	// Shared pagination types live in gen/grpc/common/v1; parse when present so
	// envelope flattening can verify sub-fields.
	commonMsgs := map[string]goStruct{}
	commonDir := filepath.Join(repoRoot, filepath.FromSlash(protoDirRel), "common", "v1")
	if _, statErr := os.Stat(commonDir); statErr == nil {
		commonMsgs, _, err = parseProtoPackage(commonDir)
		if err != nil {
			return err
		}
	}

	ob := &oasBuilder{
		pkg:         pkg,
		alias:       alias,
		protoImport: protoImport,
		humas:       humas,
		messages:    messages,
		commonMsgs:  commonMsgs,
		enums:       enums,
		respModel:   map[string]string{},
		reqModel:    map[string]string{},
		usedEnums:   map[string]bool{},
	}

	root := ob.rootModel()
	if root == "" {
		return nil // no huma model matches the domain's root proto message
	}
	ob.root = root
	ob.discover(root)
	if len(ob.respModel) == 0 && len(ob.reqModel) == 0 {
		return nil
	}

	body := ob.render()
	src := ob.assemble(body)
	out, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("format generated source: %w", err)
	}
	if err := codegen.WriteFileAtomic(filepath.Join(domainDir, outFileName), out); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

type oasBuilder struct {
	pkg         string
	alias       string
	protoImport string
	root        string // root proto message / huma model name (e.g. "User")
	humas       map[string]goStruct
	messages    map[string]goStruct
	commonMsgs  map[string]goStruct // shared gen/grpc/common/v1 messages (pagination)
	enums       map[string]protoEnum
	respModel   map[string]string // proto message -> huma response model
	reqModel    map[string]string // proto message -> huma request type
	usedEnums   map[string]bool
	needsTime   bool
}

// rootModel derives the root entity from the proto import path: the segment
// before /v1 names the domain ("user"), whose PascalCase form must exist as
// BOTH a proto message and a huma model (e.g. User). Request/response wrapper
// messages share names with huma wrapper types, so name-scanning is ambiguous;
// the import path is not.
func (ob *oasBuilder) rootModel() string {
	segs := strings.Split(ob.protoImport, "/")
	if len(segs) < 2 {
		return ""
	}
	root := codegen.ProtoGoName(segs[len(segs)-2]) // "user" -> "User"
	if _, ok := ob.messages[root]; !ok {
		return ""
	}
	if _, ok := ob.humas[root]; !ok {
		return ""
	}
	return root
}

// discover walks the response-model tree from root and the request-model tree
// from Create<Root>Request, pairing each huma type to a proto message.
func (ob *oasBuilder) discover(root string) {
	ob.walkModels(root, root, ob.respModel, map[string]bool{})
	if req := createPrefix + root + requestSuffix; ob.humas[req].name != "" {
		ob.walkModels(req, root, ob.reqModel, map[string]bool{})
	}
}

// walkModels records humaType<->protoMsg and recurses into struct-typed fields,
// pairing nested huma types with the proto message field's type.
func (ob *oasBuilder) walkModels(humaType, protoMsg string, out map[string]string, seen map[string]bool) {
	if seen[humaType] {
		return
	}
	seen[humaType] = true
	out[protoMsg] = humaType

	msgFields := fieldSet(ob.messages[protoMsg])
	for _, f := range ob.humas[humaType].fields {
		nested, ok := ob.humas[f.base]
		if !ok || nested.name == "" {
			continue
		}
		protoName := codegen.ProtoGoName(codegen.Snake(f.name))
		pf, ok := msgFields[protoName]
		if !ok || !ob.isMessage(pf.base) {
			continue
		}
		ob.walkModels(f.base, pf.base, out, seen)
	}
}

func (ob *oasBuilder) render() []byte {
	buf := &bytes.Buffer{}
	for _, msg := range sortedKeys(ob.respModel) {
		ob.renderFromProto(buf, ob.respModel[msg], msg)
	}
	for _, msg := range sortedKeys(ob.reqModel) {
		ob.renderToProto(buf, ob.reqModel[msg], msg, ob.respModel[msg])
	}
	for _, e := range ob.discoverEnvelopes() {
		ob.renderEnvelope(buf, e)
	}
	ob.renderEnums(buf)
	return buf.Bytes()
}

// envelope pairs a proto <Rpc>Response message with a huma <Rpc>Output type
// (single Body field) — the naming convention that makes wrapping derivable.
type envelope struct {
	rpc     string  // "ListUsers"
	respMsg string  // "ListUsersResponse"
	output  string  // "ListUsersOutput"
	body    goField // the Output's Body field
}

func (ob *oasBuilder) discoverEnvelopes() []envelope {
	var out []envelope
	for _, msg := range sortedKeys(ob.messages) {
		rpc, ok := strings.CutSuffix(msg, "Response")
		if !ok || rpc == "" {
			continue
		}
		o, exists := ob.humas[rpc+"Output"]
		if !exists || len(o.fields) != 1 || o.fields[0].name != "Body" {
			continue
		}
		out = append(out, envelope{rpc: rpc, respMsg: msg, output: rpc + "Output", body: o.fields[0]})
	}
	return out
}

// renderEnvelope emits <rpc>OutputFromProto(in *pb.<Rpc>Response) *<Rpc>Output.
// The Body is either a mapped model (wired through its FromProto mapper) or a
// result struct whose fields map from the response by name — including []Model
// loops and pagination flattening through common/v1 sub-messages. Getters are
// used throughout for nil-safety.
func (ob *oasBuilder) renderEnvelope(buf *bytes.Buffer, e envelope) {
	prestmts, assigns, ok := ob.envelopeBody(e)
	if !ok {
		return
	}

	fn := lowerFirst(e.rpc) + "OutputFromProto"
	fmt.Fprintf(buf, "// %s wraps %s.%s into %s.\n", fn, ob.alias, e.respMsg, e.output)
	fmt.Fprintf(buf, "func %s(in *%s.%s) *%s {\n", fn, ob.alias, e.respMsg, e.output)
	fmt.Fprintf(buf, "\tif in == nil {\n\t\treturn &%s{}\n\t}\n", e.output)
	for _, s := range prestmts {
		buf.WriteString(s)
	}
	fmt.Fprintf(buf, "\treturn &%s{\n", e.output)
	if len(assigns) == 1 && assigns[0].name == "" {
		fmt.Fprintf(buf, "\t\tBody: %s,\n", assigns[0].expr)
	} else {
		fmt.Fprintf(buf, "\t\tBody: %s{\n", e.body.base)
		for _, a := range assigns {
			fmt.Fprintf(buf, "\t\t\t%s: %s,\n", a.name, a.expr)
		}
		buf.WriteString("\t\t},\n")
	}
	buf.WriteString("\t}\n}\n\n")
}

type bodyAssign struct {
	name string // "" for a whole-Body expression
	expr string
}

// envelopeBody resolves the Body mapping. ok is false when nothing derivable.
func (ob *oasBuilder) envelopeBody(e envelope) (prestmts []string, assigns []bodyAssign, ok bool) {
	resp := ob.messages[e.respMsg]

	// Case A: Body type is itself a mapped model — find the response's single
	// field of the corresponding message type.
	for msg, model := range ob.respModel {
		if model != e.body.base {
			continue
		}
		for _, rf := range resp.fields {
			if rf.base == msg && rf.ptr {
				return nil, []bodyAssign{{expr: lowerFirst(model) + "FromProto(in.Get" + rf.name + "())"}}, true
			}
		}
	}

	// Case B: Body is a result struct; map its fields from the response.
	result, exists := ob.humas[e.body.base]
	if !exists {
		return nil, nil, false
	}
	respFields := fieldSet(resp)
	for _, bf := range result.fields {
		switch {
		case strings.HasPrefix(bf.typ, "[]"):
			stmt, expr, matched := ob.listField(bf, resp)
			if matched {
				prestmts = append(prestmts, stmt)
				assigns = append(assigns, bodyAssign{name: bf.name, expr: expr})
			}
		default:
			protoName := codegen.ProtoGoName(codegen.Snake(bf.name))
			if rf, direct := respFields[protoName]; direct {
				if expr, convOK := convertScalar(bf.typ, rf.base, "in.Get"+rf.name+"()"); convOK {
					assigns = append(assigns, bodyAssign{name: bf.name, expr: expr})
				}
				continue
			}
			if expr, flatOK := ob.flattenField(bf, protoName, resp); flatOK {
				assigns = append(assigns, bodyAssign{name: bf.name, expr: expr})
			}
		}
	}
	return prestmts, assigns, len(assigns) > 0
}

// listField maps a Body []Model field from a repeated response field via the
// model's FromProto mapper, returning the loop statement and the local var.
func (ob *oasBuilder) listField(bf goField, resp goStruct) (stmt, expr string, ok bool) {
	elem := strings.TrimPrefix(bf.typ, "[]")
	for msg, model := range ob.respModel {
		if model != elem {
			continue
		}
		for _, rf := range resp.fields {
			if rf.typ != "[]*"+msg {
				continue
			}
			local := lowerFirst(bf.name)
			stmt = "\t" + local + " := make([]" + elem + ", 0, len(in.Get" + rf.name + "()))\n" +
				"\tfor _, item := range in.Get" + rf.name + "() {\n" +
				"\t\t" + local + " = append(" + local + ", " + lowerFirst(model) + "FromProto(item))\n" +
				"\t}\n"
			return stmt, local, true
		}
	}
	return "", "", false
}

// flattenField resolves a Body scalar against sub-fields of the response's
// common/v1 message fields (e.g. Page via PaginationResponse), verified against
// the parsed common package.
func (ob *oasBuilder) flattenField(bf goField, protoName string, resp goStruct) (string, bool) {
	for _, rf := range resp.fields {
		base := rf.base
		if i := strings.LastIndex(base, "."); i >= 0 {
			base = base[i+1:]
		}
		common, ok := ob.commonMsgs[base]
		if !ok {
			continue
		}
		for _, sub := range common.fields {
			if sub.name != protoName {
				continue
			}
			if expr, convOK := convertScalar(bf.typ, sub.base, "in.Get"+rf.name+"().Get"+sub.name+"()"); convOK {
				return expr, true
			}
		}
	}
	return "", false
}

// convertScalar adapts a proto scalar to the Body field's type; unmappable
// combinations report false.
func convertScalar(dstTyp, srcBase, expr string) (string, bool) {
	switch {
	case dstTyp == srcBase:
		return expr, true
	case dstTyp == "int" && (srcBase == "int32" || srcBase == "int64"):
		return "int(" + expr + ")", true
	default:
		return "", false
	}
}

// renderFromProto emits <model>FromProto(in *pb.Msg) <Model|*Model>. The root
// model returns by value; nested models return pointers.
func (ob *oasBuilder) renderFromProto(buf *bytes.Buffer, model, msg string) {
	fn := lowerFirst(model) + "FromProto"
	ret, empty := model, model+"{}"
	if ob.isNested(msg) {
		ret, empty = "*"+model, "nil"
	}
	fmt.Fprintf(buf, "// %s maps %s.%s to %s.\n", fn, ob.alias, msg, model)
	fmt.Fprintf(buf, "func %s(in *%s.%s) %s {\n\tif in == nil {\n\t\treturn %s\n\t}\n", fn, ob.alias, msg, ret, empty)
	if strings.HasPrefix(ret, "*") {
		fmt.Fprintf(buf, "\treturn &%s{\n", model)
	} else {
		fmt.Fprintf(buf, "\treturn %s{\n", model)
	}
	assigns := map[string]string{}
	msgFields := fieldSet(ob.messages[msg])
	for _, f := range ob.humas[model].fields {
		if strings.HasPrefix(f.typ, "[]") {
			continue // slices are handled by envelope/list mappers
		}
		protoName := codegen.ProtoGoName(codegen.Snake(f.name))
		pf, ok := msgFields[protoName]
		if !ok {
			continue
		}
		assigns[f.name] = ob.fromExpr(f, pf, "in."+protoName)
	}
	for _, name := range sortedKeys(assigns) {
		fmt.Fprintf(buf, "\t\t%s: %s,\n", name, assigns[name])
	}
	buf.WriteString("\t}\n}\n\n")
}

// renderToProto emits <respModel>ToProto(src <Req|*Req>) *pb.Msg.
func (ob *oasBuilder) renderToProto(buf *bytes.Buffer, reqType, msg, respModel string) {
	name := respModel
	if name == "" {
		name = msg
	}
	fn := lowerFirst(name) + "ToProto"
	param := reqType
	deref := "src."
	if ob.isNested(msg) {
		param = "*" + reqType
	}
	fmt.Fprintf(buf, "// %s maps %s to %s.%s.\n", fn, reqType, ob.alias, msg)
	fmt.Fprintf(buf, "func %s(src %s) *%s.%s {\n", fn, param, ob.alias, msg)
	if strings.HasPrefix(param, "*") {
		buf.WriteString("\tif src == nil {\n\t\treturn nil\n\t}\n")
	}
	fmt.Fprintf(buf, "\treturn &%s.%s{\n", ob.alias, msg)
	assigns := map[string]string{}
	msgFields := fieldSet(ob.messages[msg])
	for _, f := range ob.humas[reqType].fields {
		if strings.HasPrefix(f.typ, "[]") {
			continue // slices are handled by envelope/list mappers
		}
		protoName := codegen.ProtoGoName(codegen.Snake(f.name))
		pf, ok := msgFields[protoName]
		if !ok {
			continue
		}
		assigns[protoName] = ob.toExpr(pf, deref+f.name)
	}
	for _, pn := range sortedKeys(assigns) {
		fmt.Fprintf(buf, "\t\t%s: %s,\n", pn, assigns[pn])
	}
	buf.WriteString("\t}\n}\n\n")
}

func (ob *oasBuilder) fromExpr(dst, pf goField, access string) string {
	switch {
	case ob.isEnum(pf.base):
		ob.usedEnums[pf.base] = true
		return lowerFirst(pf.base) + "FromProto(" + access + ")"
	case pf.typ == timestampType && dst.base == timeType:
		ob.needsTime = true
		return "protoutil.TimeFromProto(" + access + ")"
	case ob.isMessage(pf.base) && pf.ptr:
		return lowerFirst(ob.respModel[pf.base]) + "FromProto(" + access + ")"
	default:
		return access
	}
}

func (ob *oasBuilder) toExpr(pf goField, access string) string {
	switch {
	case ob.isEnum(pf.base):
		ob.usedEnums[pf.base] = true
		return "toProto" + pf.base + "(" + access + ")"
	case ob.isMessage(pf.base) && pf.ptr:
		return lowerFirst(ob.respModel[pf.base]) + "ToProto(" + access + ")"
	default:
		return access
	}
}

func (ob *oasBuilder) renderEnums(buf *bytes.Buffer) {
	for _, name := range sortedKeys(boolSet(ob.usedEnums)) {
		e := ob.enums[name]
		fmt.Fprintf(buf, "// toProto%s maps a string to the %s.%s enum.\n", name, ob.alias, name)
		fmt.Fprintf(buf, "func toProto%s(value string) %s.%s {\n\tswitch value {\n", name, ob.alias, name)
		for _, v := range e.values {
			fmt.Fprintf(buf, "\tcase %q:\n\t\treturn %s.%s\n", v.str, ob.alias, v.constName)
		}
		fmt.Fprintf(buf, "\tdefault:\n\t\treturn %s.%s\n\t}\n}\n\n", ob.alias, unspecifiedConst(name))

		fmt.Fprintf(buf, "// %sFromProto maps the %s.%s enum to a string.\n", lowerFirst(name), ob.alias, name)
		fmt.Fprintf(buf, "func %sFromProto(value %s.%s) string {\n\tswitch value {\n", lowerFirst(name), ob.alias, name)
		for _, v := range e.values {
			fmt.Fprintf(buf, "\tcase %s.%s:\n\t\treturn %q\n", ob.alias, v.constName, v.str)
		}
		buf.WriteString("\tdefault:\n\t\treturn \"\"\n\t}\n}\n\n")
	}
}

func (ob *oasBuilder) assemble(body []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by mapgen map; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", ob.pkg)
	buf.WriteString("import (\n")
	if ob.needsTime {
		fmt.Fprintf(&buf, "\t%q\n\n", protoutilPkg)
	}
	fmt.Fprintf(&buf, "\t%s %q\n", ob.alias, ob.protoImport)
	buf.WriteString(")\n\n")
	buf.Write(body)
	return buf.Bytes()
}

func (ob *oasBuilder) isEnum(n string) bool    { _, ok := ob.enums[n]; return ok }
func (ob *oasBuilder) isMessage(n string) bool { _, ok := ob.messages[n]; return ok }

// isNested reports whether a proto message is below the domain root; nested
// mappers use pointer semantics, root mappers pass/return by value.
func (ob *oasBuilder) isNested(msg string) bool {
	return msg != ob.root
}

// findProtoImport scans the domain's Go files for an import matching
// <module>/gen/grpc/<x>/v1 (skipping the shared common package) and returns its
// dir under repoRoot, its alias, and the import path.
func findProtoImport(repoRoot, domainDir, module string) (protoDir, alias, importPath string, err error) {
	files, err := goFiles(domainDir)
	if err != nil {
		return "", "", "", err
	}
	prefix := module + "/" + protoDirRel + "/"
	for _, path := range files {
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			return "", "", "", fmt.Errorf("parse imports %s: %w", path, perr)
		}
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, "/v1") {
				continue
			}
			if strings.HasSuffix(p, "/common/v1") {
				continue // shared types, not the domain's own proto package
			}
			a := importAlias(imp, p)
			rel := strings.TrimPrefix(p, module+"/")
			return filepath.Join(repoRoot, filepath.FromSlash(rel)), a, p, nil
		}
	}
	return "", "", "", nil
}

func importAlias(imp *ast.ImportSpec, path string) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	base := path[strings.LastIndex(path, "/")+1:]
	// e.g. ".../user/v1" -> alias derives from the segment before /v1
	segs := strings.Split(path, "/")
	if len(segs) >= 2 {
		return segs[len(segs)-2] + base
	}
	return base
}
