// Package filtergen implements the "mapgen filter" subcommand. For every feature
// package under internal/feature/<f>/ whose root model (PascalCase of the feature
// name) has a generated column map, it emits filter_generated.go: thin
// applyFilter/applyOrderBy wrappers over orm.ApplyFilter/orm.ApplyOrderBy plus a
// registry of custom FilterExpr functions collected from //mapgen:filter
// directives. This lets a feature keep the auto-generated column mapping while
// backing virtual/composite columns with hand-written SQL, instead of abandoning
// the generated path entirely.
package filtergen

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

	"github.com/kitti12911/lib-orm/v4/internal/codegen"
)

const (
	featureDirRel = "internal/feature"
	modelDirRel   = "internal/database"
	fieldmapAlias = "fieldmap"
	ormModulePath = "github.com/kitti12911/lib-orm/v4"
	bunModulePath = "github.com/uptrace/bun"
	outFileName   = "filter_generated.go"
)

// Run generates filter helpers for every eligible feature. -C sets the repo root.
func Run(args []string) error {
	fs := flag.NewFlagSet("mapgen filter", flag.ContinueOnError)
	dir := fs.String("C", ".", "repo root directory")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	mod, err := readModulePaths(*dir)
	if err != nil {
		return err
	}

	roots, err := rootModels(filepath.Join(*dir, modelDirRel))
	if err != nil {
		return err
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
		feature := entry.Name()
		root := codegen.ProtoGoName(feature) // PascalCase; "user" -> "User"
		if !roots[root] {
			continue
		}

		featureDir := filepath.Join(featureRoot, feature)
		spec, err := parseFeature(featureDir)
		if err != nil {
			return err
		}
		if spec.ignore {
			continue
		}

		if err := writeFilterFile(featureDir, spec.pkg, root, mod, spec.customs); err != nil {
			return err
		}
	}

	return nil
}

type modulePaths struct {
	module  string
	libUtil string
}

func readModulePaths(dir string) (modulePaths, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return modulePaths{}, fmt.Errorf("read go.mod: %w", err)
	}

	var mod modulePaths
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "module" {
			mod.module = fields[1]
			continue
		}
		if len(fields) >= 1 && strings.HasPrefix(fields[0], "github.com/kitti12911/lib-util/v") {
			mod.libUtil = fields[0]
		}
	}

	if mod.module == "" {
		return modulePaths{}, errors.New("module path not found in go.mod")
	}
	if mod.libUtil == "" {
		return modulePaths{}, errors.New("lib-util require not found in go.mod")
	}
	return mod, nil
}

// rootModels returns the set of root model names (PascalCase) that have a
// generated <Root>Columns map: a model with a bun table tag that no other model
// forward-relates to (has-one/has-many/many-to-many). Belongs-to is ignored.
func rootModels(modelDir string) (map[string]bool, error) {
	entries, err := os.ReadDir(modelDir)
	if err != nil {
		return nil, fmt.Errorf("read model directory: %w", err)
	}

	models := map[string]bool{}
	child := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if err := scanModelFile(filepath.Join(modelDir, name), models, child); err != nil {
			return nil, err
		}
	}

	roots := map[string]bool{}
	for name := range models {
		if !child[name] {
			roots[name] = true
		}
	}
	return roots, nil
}

func scanModelFile(path string, models, child map[string]bool) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse model file: %w", err)
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
			scanModelStruct(typeSpec.Name.Name, strct, models, child)
		}
	}
	return nil
}

func scanModelStruct(name string, strct *ast.StructType, models, child map[string]bool) {
	isModel := false
	for _, field := range strct.Fields.List {
		bunTag := codegen.StructTag(field).Get("bun")
		if bunTag == "" {
			continue
		}
		if len(field.Names) == 0 && strings.Contains(bunTag, "table:") {
			isModel = true
			continue
		}
		if strings.Contains(bunTag, "rel:") && forwardRelation(bunTag) {
			child[relationModel(field.Type)] = true
		}
	}
	if isModel {
		models[name] = true
	}
}

func forwardRelation(tag string) bool {
	for opt := range strings.SplitSeq(tag, ",") {
		switch opt {
		case "rel:has-one", "rel:has-many", "rel:many-to-many":
			return true
		}
	}
	return false
}

func relationModel(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return relationModel(t.X)
	case *ast.ArrayType:
		return relationModel(t.Elt)
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	default:
		return ""
	}
}

type customFilter struct {
	col string
	fn  string
}

type featureSpec struct {
	pkg     string
	ignore  bool
	customs []customFilter
}

func parseFeature(dir string) (featureSpec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return featureSpec{}, fmt.Errorf("read feature directory: %w", err)
	}

	var spec featureSpec
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == outFileName {
			continue
		}
		if err := scanFeatureFile(filepath.Join(dir, name), &spec); err != nil {
			return featureSpec{}, err
		}
	}

	sort.Slice(spec.customs, func(i, j int) bool { return spec.customs[i].col < spec.customs[j].col })
	return spec, nil
}

func scanFeatureFile(path string, spec *featureSpec) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse feature file: %w", err)
	}

	spec.pkg = file.Name.Name
	if file.Doc != nil && directivePresent(file.Doc, "ignore") {
		spec.ignore = true
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Doc == nil {
			continue
		}
		if col, ok := filterCol(fn.Doc); ok {
			spec.customs = append(spec.customs, customFilter{col: col, fn: fn.Name.Name})
		}
	}
	return nil
}

func directivePresent(group *ast.CommentGroup, name string) bool {
	for _, c := range group.List {
		if strings.Contains(c.Text, "mapgen:"+name) {
			return true
		}
	}
	return false
}

// filterCol extracts the column from a `//mapgen:filter col=<name>` directive.
func filterCol(group *ast.CommentGroup) (string, bool) {
	for _, c := range group.List {
		text := strings.TrimPrefix(strings.TrimPrefix(c.Text, "//"), " ")
		fields := strings.Fields(text)
		if len(fields) < 2 || fields[0] != "mapgen:filter" {
			continue
		}
		for _, f := range fields[1:] {
			if col, ok := strings.CutPrefix(f, "col="); ok && col != "" {
				return col, true
			}
		}
	}
	return "", false
}

func writeFilterFile(dir, pkg, root string, mod modulePaths, customs []customFilter) error {
	columnsVar := fieldmapAlias + "." + root + "Columns"
	registryVar := lowerFirst(root) + "CustomFilters"

	var buf bytes.Buffer
	buf.WriteString("// Code generated by mapgen filter; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", pkg)

	buf.WriteString("import (\n")
	fmt.Fprintf(&buf, "\t%q\n\n", mod.libUtil+"/apperror")
	fmt.Fprintf(&buf, "\t%s %q\n\n", fieldmapAlias, mod.module+"/gen/database")
	fmt.Fprintf(&buf, "\torm %q\n", ormModulePath)
	fmt.Fprintf(&buf, "\t%q\n", bunModulePath)
	buf.WriteString(")\n\n")

	fmt.Fprintf(&buf, "var %s = map[string]orm.FilterExpr{\n", registryVar)
	for _, c := range customs {
		fmt.Fprintf(&buf, "\t%q: %s,\n", c.col, c.fn)
	}
	buf.WriteString("}\n\n")

	fmt.Fprintf(&buf, "func applyFilter(query *bun.SelectQuery, filter orm.Filter) error {\n")
	fmt.Fprintf(&buf, "\tif err := orm.ApplyFilter(query, filter, %s, %s); err != nil {\n", columnsVar, registryVar)
	fmt.Fprintf(&buf, "\t\treturn apperror.InvalidInput(%q, err)\n", "invalid filter")
	buf.WriteString("\t}\n\treturn nil\n}\n\n")

	fmt.Fprintf(&buf, "func applyOrderBy(query *bun.SelectQuery, orderBy []orm.OrderBy) error {\n")
	fmt.Fprintf(&buf, "\tif err := orm.ApplyOrderBy(query, orderBy, %s); err != nil {\n", columnsVar)
	fmt.Fprintf(&buf, "\t\treturn apperror.InvalidInput(%q, err)\n", "invalid order by")
	buf.WriteString("\t}\n\treturn nil\n}\n")

	out, err := format.Source(buf.Bytes())
	if err != nil {
		return fmt.Errorf("format generated source: %w", err)
	}
	if err := codegen.WriteFileAtomic(filepath.Join(dir, outFileName), out); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
