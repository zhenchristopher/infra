//go:build linux

package fc

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirecrackerExitDetails(t *testing.T) {
	t.Parallel()

	result, signal, exitCode := firecrackerExitDetails(nil)
	assert.Equal(t, "success", result)
	assert.Equal(t, "none", signal)
	assert.Equal(t, 0, exitCode)

	cmd := exec.Command("sh", "-c", "exit 7")
	err := cmd.Run()
	require.Error(t, err)

	result, signal, exitCode = firecrackerExitDetails(err)
	assert.Equal(t, "error", result)
	assert.Equal(t, "none", signal)
	assert.Equal(t, 7, exitCode)
}
