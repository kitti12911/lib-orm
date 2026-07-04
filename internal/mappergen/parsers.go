package mappergen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/kitti12911/lib-orm/v4/internal/codegen"
)

func readModule(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("read go.mod: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("module path not found in go.mod")
}

// goFiles returns the parseable .go files in dir, skipping tests and the
// generated mapper output.
func goFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read directory: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == outFileName {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

// parseGoStructs parses every struct type in dir into a goStruct.
func parseGoStructs(dir string) (map[string]goStruct, error) {
	files, err := goFiles(dir)
	if err != nil {
		return nil, err
	}
	structs := map[string]goStruct{}
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		eachStruct(file, func(name string, st *ast.StructType) {
			structs[name] = structFields(name, st)
		})
	}
	return structs, nil
}

// parseFeatureStructs parses a feature package. It returns every struct, whether
// the package is //mapgen:ignore'd, and captures //mapgen:proto=<Msg> overrides
// stored on the struct's override field.
func parseFeatureStructs(dir string) (pkg string, structs map[string]goStruct, ignore bool, err error) {
	files, err := goFiles(dir)
	if err != nil {
		return "", nil, false, fmt.Errorf("read feature directory: %w", err)
	}
	structs = map[string]goStruct{}
	for _, path := range files {
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			return "", nil, false, fmt.Errorf("parse %s: %w", path, perr)
		}
		pkg = file.Name.Name
		if file.Doc != nil && commentHas(file.Doc, "mapgen:ignore") {
			ignore = true
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				s := structFields(ts.Name.Name, st)
				if override := protoOverride(gen.Doc); override != "" {
					s.protoOverride = override
				}
				structs[ts.Name.Name] = s
			}
		}
	}
	return pkg, structs, ignore, nil
}

// parseRelations returns each bun model's related model names (any relation
// direction), used to find the models reachable from a feature root.
func parseRelations(dir string) map[string][]string {
	files, _ := goFiles(dir)
	rels := map[string][]string{}
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		eachStruct(file, func(name string, st *ast.StructType) {
			for _, f := range st.Fields.List {
				bunTag := codegen.StructTag(f).Get("bun")
				if !strings.Contains(bunTag, "rel:") {
					continue
				}
				if related := baseTypeName(f.Type); related != "" {
					rels[name] = append(rels[name], related)
				}
			}
		})
	}
	return rels
}

// parseProtoPackage parses the generated proto Go types in dir into message
// structs and enums.
func parseProtoPackage(dir string) (messages map[string]goStruct, enums map[string]protoEnum, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read proto directory: %w", err)
	}

	messages = map[string]goStruct{}
	enums = map[string]protoEnum{}
	enumTypes := map[string]bool{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".pb.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", name, err)
		}
		collectTypes(file, messages, enumTypes) // messages + enum type decls
		for _, decl := range file.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				collectEnumValues(gen, enumTypes, enums)
			}
		}
	}

	return messages, enums, nil
}

// collectTypes records struct messages and enum type declarations (type X int32).
func collectTypes(file *ast.File, messages map[string]goStruct, enumTypes map[string]bool) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			switch t := ts.Type.(type) {
			case *ast.Ident:
				if t.Name == "int32" {
					enumTypes[ts.Name.Name] = true
				}
			case *ast.StructType:
				messages[ts.Name.Name] = structFields(ts.Name.Name, t)
			}
		}
	}
}

func collectEnumValues(gen *ast.GenDecl, enumTypes map[string]bool, enums map[string]protoEnum) {
	for _, spec := range gen.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok || vs.Type == nil || len(vs.Names) != 1 || len(vs.Values) != 1 {
			continue
		}
		enumType, ok := vs.Type.(*ast.Ident)
		if !ok || !enumTypes[enumType.Name] {
			continue
		}
		lit, ok := vs.Values[0].(*ast.BasicLit)
		if !ok || lit.Value == "0" {
			continue // skip the zero/unspecified value
		}
		constName := vs.Names[0].Name
		valueName := strings.TrimPrefix(constName, enumType.Name+"_")
		str := strings.ToLower(strings.TrimPrefix(valueName, strings.ToUpper(codegen.Snake(enumType.Name))+"_"))
		e := enums[enumType.Name]
		e.name = enumType.Name
		e.values = append(e.values, protoEnumVal{constName: constName, str: str})
		enums[enumType.Name] = e
	}
}

func eachStruct(file *ast.File, fn func(name string, st *ast.StructType)) {
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				fn(ts.Name.Name, st)
			}
		}
	}
}

// structFields extracts exported named fields (skipping embedded/unexported).
func structFields(name string, st *ast.StructType) goStruct {
	s := goStruct{name: name}
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 || !ast.IsExported(f.Names[0].Name) {
			continue
		}
		typ := typeString(f.Type)
		if typ == "" {
			continue
		}
		s.fields = append(s.fields, goField{
			name: f.Names[0].Name,
			typ:  typ,
			base: strings.TrimPrefix(typ, "*"),
			ptr:  strings.HasPrefix(typ, "*"),
		})
	}
	return s
}

// typeString flattens a type expression to a canonical string. Only the shapes
// the mapper cares about are represented precisely; anything else returns "".
func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		inner := typeString(t.X)
		if inner == "" {
			return ""
		}
		return "*" + inner
	case *ast.ArrayType:
		if t.Len != nil {
			return "" // fixed-size arrays are out of scope
		}
		inner := typeString(t.Elt)
		if inner == "" {
			return ""
		}
		return "[]" + inner
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok {
			return ""
		}
		return pkg.Name + "." + t.Sel.Name
	default:
		return ""
	}
}

func baseTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return baseTypeName(t.X)
	case *ast.ArrayType:
		return baseTypeName(t.Elt)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	default:
		return ""
	}
}

func commentHas(group *ast.CommentGroup, needle string) bool {
	if group == nil {
		return false
	}
	for _, c := range group.List {
		if strings.Contains(c.Text, needle) {
			return true
		}
	}
	return false
}

func protoOverride(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	for _, c := range group.List {
		text := strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), " ")
		if v, ok := strings.CutPrefix(text, "mapgen:proto="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
