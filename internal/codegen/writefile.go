// Package codegen holds the shared engine pieces used by the mapgen
// subcommands (proto, fields, patch): atomic file writing, Go-source parsing
// helpers, and identifier-naming utilities. Each generator was previously a
// standalone command with its own copy of these; consolidating them here keeps
// behavior identical across all three.
package codegen

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path by first writing to a temp file in the
// same directory and renaming on success, so a failed write never leaves a
// partial file at path. The output file mode is 0o644.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".codegen-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // best-effort cleanup; rename removes the file on success

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck // already failing
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close() //nolint:errcheck // already failing
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
