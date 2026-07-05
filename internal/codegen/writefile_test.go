package codegen

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "out.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, WriteFileAtomic(path, []byte("hello")))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))

	require.NoError(t, WriteFileAtomic(path, []byte("world")))
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "world", string(data))
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	err := WriteFileAtomic(filepath.Join(t.TempDir(), "nope", "out.txt"), []byte("x"))
	require.Error(t, err)
}

func TestWriteFileAtomicRenameError(t *testing.T) {
	// path is an existing directory, so renaming the temp file over it fails.
	dir := filepath.Join(t.TempDir(), "target-dir")
	require.NoError(t, os.Mkdir(dir, 0o755))
	err := WriteFileAtomic(dir, []byte("x"))
	require.Error(t, err)
}
