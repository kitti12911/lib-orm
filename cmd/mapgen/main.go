// Command mapgen is the unified, zero-config code generator for lib-orm
// consumers. Every subcommand discovers its inputs by convention from the repo
// root (-C, default "."); none takes a config file.
//
//	mapgen map      # struct <-> proto mappers (+ enum bridges + model constructors)
//	mapgen fields   # bun field/column maps
//	mapgen patch    # partial-update dispatchers (+ patchData structs)
//	mapgen filter   # filter/order-by helpers (+ custom-filter registry)
//
// They share the AST, naming, and file-writing helpers in internal/codegen.
package main

import (
	"fmt"
	"os"

	"github.com/kitti12911/lib-orm/v4/internal/fieldmap"
	"github.com/kitti12911/lib-orm/v4/internal/filtergen"
	"github.com/kitti12911/lib-orm/v4/internal/mappergen"
	"github.com/kitti12911/lib-orm/v4/internal/patchfield"
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
	case "map":
		run = mappergen.Run
	case "fields":
		run = fieldmap.Run
	case "patch":
		run = patchfield.Run
	case "filter":
		run = filtergen.Run
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

subcommands (all zero-config; -C sets the repo root):
  map     generate Go struct <-> proto mapper functions, enum bridges, and params->model constructors
  fields  generate bun field/column maps
  patch   generate partial-update field dispatchers + patchData structs
  filter  generate filter/order-by helpers + custom-filter registry
`)
}
