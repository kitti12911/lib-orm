package orm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUUIDScanMSSQLBytes(t *testing.T) {
	raw := []byte{0x10, 0x32, 0x54, 0x76, 0x98, 0xba, 0xdc, 0xfe, 0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}

	var u UUID
	require.NoError(t, u.Scan(raw))
	assert.Equal(t, "76543210-ba98-fedc-0123-456789abcdef", u.String())
}

func TestUUIDScanStringIsLowercased(t *testing.T) {
	var u UUID
	require.NoError(t, u.Scan("A1B2C3D4-E5F6-7890-ABCD-EF0123456789"))
	assert.Equal(t, "a1b2c3d4-e5f6-7890-abcd-ef0123456789", u.String())
}

func TestUUIDScanNilAndEmpty(t *testing.T) {
	var u UUID
	require.NoError(t, u.Scan(nil))
	assert.Equal(t, "", u.String())

	require.NoError(t, u.Scan(""))
	assert.Equal(t, "", u.String())
}

func TestUUIDValueEmptyIsNull(t *testing.T) {
	v, err := UUID("").Value()
	require.NoError(t, err)
	assert.Nil(t, v)
}

func TestUUIDValueNormalizes(t *testing.T) {
	v, err := UUID("A1B2C3D4-E5F6-7890-ABCD-EF0123456789").Value()
	require.NoError(t, err)
	assert.Equal(t, "a1b2c3d4-e5f6-7890-abcd-ef0123456789", v)
}

func TestUUIDInvalidErrors(t *testing.T) {
	var u UUID
	assert.Error(t, u.Scan("not-a-uuid"))

	_, err := UUID("not-a-uuid").Value()
	assert.Error(t, err)
}
