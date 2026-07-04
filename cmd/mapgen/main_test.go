package main

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMainExitsOnError re-execs the test binary so main() runs for real, then
// asserts the process exits non-zero with the expected stderr prefix. The
// MAPGEN_TEST_MAIN env var carries the argv to replay.
func TestMainExitsOnError(t *testing.T) {
	if args := os.Getenv("MAPGEN_TEST_MAIN"); args != "" {
		os.Args = []string{"mapgen", args}
		main()
		return
	}

	cases := map[string]struct {
		arg      string
		wantCode int
		wantErr  string
	}{
		"unknown subcommand": {arg: "bogus", wantCode: 2, wantErr: "unknown subcommand"},
		"subcommand error":   {arg: "proto", wantCode: 1, wantErr: "mapgen proto:"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestMainExitsOnError") //nolint:gosec // os.Args[0] is the test binary path
			cmd.Env = append(os.Environ(), "MAPGEN_TEST_MAIN="+tc.arg)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()

			var exitErr *exec.ExitError
			require.ErrorAs(t, err, &exitErr)
			assert.Equal(t, tc.wantCode, exitErr.ExitCode())
			assert.Contains(t, stderr.String(), tc.wantErr)
		})
	}
}

func TestNoSubcommandExitsTwo(t *testing.T) {
	if os.Getenv("MAPGEN_TEST_NOARGS") == "1" {
		os.Args = []string{"mapgen"}
		main()
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestNoSubcommandExitsTwo") //nolint:gosec // os.Args[0] is the test binary path
	cmd.Env = append(os.Environ(), "MAPGEN_TEST_NOARGS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 2, exitErr.ExitCode())
	assert.Contains(t, stderr.String(), "usage: mapgen")
}
