package logentries

import (
	"testing"

	"github.com/docker/docker/daemon/logger"
	"github.com/stretchr/testify/require"
)

func TestNewWithEmptyOptions(t *testing.T) {
	_, err := New(logger.Info{})
	require.NoError(t, err)
}
