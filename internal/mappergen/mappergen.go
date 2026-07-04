// Package mappergen implements the "mapgen map" subcommand: it generates
// struct<->proto mapper functions with zero configuration by reading BOTH sides
// of every mapping — the Go structs (bun models under internal/database and
// Create<X>Params structs under internal/feature/<f>) and the generated proto Go
// types under gen/grpc/<f>/v1. Reading the proto side lets the generator derive
// the field set by intersection (no exclude lists), auto-wire nested mappers,
// and auto-generate string<->enum bridges — everything the YAML-driven
// predecessor needed configured by hand.
//
// For each feature package it emits internal/feature/<f>/mapper_generated.go:
//
//   - to_proto:   toProto<Model>(src *database.<Model>) *<pb>.<Model>   (bun -> proto)
//     for the feature root model and every relation model reachable from it.
//   - from_proto: <lowerParams>FromProto(in *<pb>.<Msg>) <Params>       (proto -> params)
//     for every Create<X>Params struct; the root params returns by value, nested
//     params return pointers.
//   - enum bridges: toProto<Enum> / <lowerEnum>FromProto for every proto enum a
//     mapped string field targets.
//
// Directives (comments, never config): //mapgen:ignore on a package skips it;
// //mapgen:proto=<Message> on a params struct overrides the derived proto type.
package mappergen

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"

	"github.com/kitti12911/lib-orm/v4/internal/codegen"
)

const (
	featureDirRel = "internal/feature"
	modelDirRel   = "internal/database"
	protoDirRel   = "gen/grpc"
	outFileName   = "mapper_generated.go"

	timestampType = "*timestamppb.Timestamp"
	timestampPkg  = "google.golang.org/protobuf/types/known/timestamppb"
)

// goStruct is a parsed Go struct: an ordered list of exported fields with their
// flattened type strings.
type goStruct struct {
	name          string
	fields        []goField
	protoOverride string // from //mapgen:proto=<Message>
}

type goField struct {
	name string
	typ  string // flattened, e.g. "string", "*string", "time.Time", "UserStatus", "*UserProfile"
	base string // type name without leading * (e.g. "UserProfile", "UserStatus", "string")
	ptr  bool
}

// protoEnum captures a proto enum and its non-zero values as lowercase strings
// keyed by the Go const identifier.
type protoEnum struct {
	name   string         // e.g. "UserStatus"
	values []protoEnumVal // non-zero, in declared order
}

type protoEnumVal struct {
	constName string // e.g. "UserStatus_USER_STATUS_ACTIVE"
	str       string // e.g. "active"
}

// Run generates struct<->proto mappers with zero config. It auto-detects the
// layout: a huma REST gateway (internal/api/<domain>/v1) uses oas mode; a
// gRPC service (internal/database + internal/feature) uses the bun mode below.
// -C sets the repo root.
func Run(args []string) error {
	fs := flag.NewFlagSet("mapgen map", flag.ContinueOnError)
	dir := fs.String("C", ".", "repo root directory")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	module, err := readModule(*dir)
	if err != nil {
		return err
	}

	if _, statErr := os.Stat(filepath.Join(*dir, apiDirRel)); statErr == nil {
		return runOAS(*dir, module)
	}

	models, err := parseGoStructs(filepath.Join(*dir, modelDirRel))
	if err != nil {
		return fmt.Errorf("parse models: %w", err)
	}
	relations := parseRelations(filepath.Join(*dir, modelDirRel))

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
		featureDir := filepath.Join(featureRoot, feature)
		protoDir := filepath.Join(*dir, protoDirRel, feature, "v1")

		g := &generator{
			module:     module,
			feature:    feature,
			featureDir: featureDir,
			protoDir:   protoDir,
			models:     models,
			relations:  relations,
		}
		if err := g.run(); err != nil {
			return fmt.Errorf("feature %s: %w", feature, err)
		}
	}
	return nil
}

type generator struct {
	module     string
	feature    string
	featureDir string
	protoDir   string
	models     map[string]goStruct
	relations  map[string][]string // model -> related model names
}

func (g *generator) run() error {
	// Skip features with no proto package.
	if _, err := os.Stat(g.protoDir); err != nil {
		return nil //nolint:nilerr // a feature without protos has nothing to map
	}

	pkg, params, ignore, err := parseFeatureStructs(g.featureDir)
	if err != nil {
		return err
	}
	if ignore {
		return nil
	}

	messages, enums, err := parseProtoPackage(g.protoDir)
	if err != nil {
		return err
	}

	root := codegen.ProtoGoName(g.feature) // "user" -> "User"
	if _, ok := g.models[root]; !ok {
		return nil // no root model; nothing to generate for this feature
	}

	b := &builder{
		g:         g,
		pkg:       pkg,
		params:    params,
		messages:  messages,
		enums:     enums,
		toProto:   map[string]string{},
		fromProto: map[string]string{},
		usedEnums: map[string]bool{},
	}

	// Discover to_proto targets: root + reachable relation models with a proto message.
	toTargets := b.discoverToTargets(root)
	// Discover from_proto targets: Create<X>Params -> <root><X>.
	fromTargets := b.discoverFromTargets(params, root)

	if len(toTargets) == 0 && len(fromTargets) == 0 {
		return nil
	}

	// Register func names before rendering so nested wiring can reference them.
	for _, t := range toTargets {
		b.toProto[t.protoType] = "toProto" + t.protoType
	}
	for _, t := range fromTargets {
		b.fromProto[t.protoType] = lowerFirst(t.goType) + "FromProto"
	}

	body := &bytes.Buffer{}
	for _, t := range toTargets {
		b.renderToProto(body, t)
	}
	for _, t := range fromTargets {
		b.renderFromProto(body, t)
	}
	b.renderEnumBridges(body)

	src := b.assemble(body.Bytes())
	out, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("format generated source: %w", err)
	}
	if err := codegen.WriteFileAtomic(filepath.Join(g.featureDir, outFileName), out); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
