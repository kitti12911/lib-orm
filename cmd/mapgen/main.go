// Command mapgen is the unified code generator for lib-orm consumers. It
// bundles three subcommands that previously shipped as separate binaries:
//
//	mapgen proto  -config protomapgen.yaml   # Go struct <-> proto mappers
//	mapgen fields -root User -model-dir ...   # bun field/column maps
//	mapgen patch  -config patchfields.yaml    # partial-update dispatchers
//
// Each subcommand keeps its own flags and config schema; they share the AST,
// naming, and file-writing helpers in internal/codegen.
package main

import (
	"fmt"
	"os"

	"github.com/kitti12911/lib-orm/v3/internal/fieldmap"
	"github.com/kitti12911/lib-orm/v3/internal/patchfield"
	"github.com/kitti12911/lib-orm/v3/internal/protomap"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	sub := os.Args[1]
	args := os.Args[2:]

	var (
		run func([]string) error
		ok  = true
	)
	switch sub {
	case "proto":
		run = protomap.Run
	case "fields":
		run = fieldmap.Run
	case "patch":
		run = patchfield.Run
	default:
		ok = false
	}

	if !ok {
		fmt.Fprintf(os.Stderr, "mapgen: unknown subcommand %q\n", sub)
		usage()
		os.Exit(2)
	}

	if err := run(args); err != nil {
		fmt.Fprintf(os.Stderr, "mapgen %s: %v\n", sub, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: mapgen <subcommand> [flags]

subcommands:
  proto   generate Go struct <-> proto mapper functions (-config)
  fields  generate bun field/column maps (-root, -model-dir, -out, -package)
  patch   generate partial-update field dispatchers (-config)
`)
}
