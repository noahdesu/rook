package version

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestToString(t *testing.T) {
	assert.Equal(t, "14.0.0 nautilus", fmt.Sprintf("%s", &Nautilus))
	assert.Equal(t, "13.0.0 mimic", fmt.Sprintf("%s", &Mimic))
	assert.Equal(t, "12.0.0 luminous", fmt.Sprintf("%s", &Luminous))

	expected := fmt.Sprintf("-1.0.0 %s", unknownVersionString)
	assert.Equal(t, expected, fmt.Sprintf("%s", &CephVersion{-1, 0, 0}))
}

func TestReleaseName(t *testing.T) {
	assert.Equal(t, "nautilus", Nautilus.ReleaseName())
	assert.Equal(t, "mimic", Mimic.ReleaseName())
	assert.Equal(t, "luminous", Luminous.ReleaseName())
	ver := CephVersion{-1, 0, 0}
	assert.Equal(t, unknownVersionString, ver.ReleaseName())
}

func extractVersionHelper(t *testing.T, text string, major, minor, extra int) {
	v, err := ExtractCephVersion(text)
	if assert.NoError(t, err) {
		assert.Equal(t, v, CephVersion{major, minor, extra})
	}
}

func TestExtractVersion(t *testing.T) {
	// release build
	v0c := "ceph version 12.2.8 (ae699615bac534ea496ee965ac6192cb7e0e07c0) luminous (stable)"
	v0d := `
root@7a97f5a78bc6:/# ceph --version
ceph version 12.2.8 (ae699615bac534ea496ee965ac6192cb7e0e07c0) luminous (stable)
`
	extractVersionHelper(t, v0c, 12, 2, 8)
	extractVersionHelper(t, v0d, 12, 2, 8)

	// development build
	v1c := "ceph version 14.1.33-403-g7ba6bece41"
	v1d := `
[nwatkins@smash build]$ bin/ceph --version
*** DEVELOPER MODE: setting PATH, PYTHONPATH and LD_LIBRARY_PATH ***
ceph version 14.1.33-403-g7ba6bece41
(7ba6bece4187eda5d05a9b84211fe6ba8dd287bd) nautilus (rc)
`
	extractVersionHelper(t, v1c, 14, 1, 33)
	extractVersionHelper(t, v1d, 14, 1, 33)

	// build without git version info
	v2c := "ceph version Development (no_version) nautilus (rc)"
	v2d := `
[nwatkins@daq build]$ bin/ceph --version
*** DEVELOPER MODE: setting PATH, PYTHONPATH and LD_LIBRARY_PATH ***
ceph version Development (no_version) nautilus (rc)
`
	v, err := ExtractCephVersion(v2c)
	assert.Error(t, err)
	assert.Equal(t, v, CephVersion{})

	v, err = ExtractCephVersion(v2d)
	assert.Error(t, err)
	assert.Equal(t, v, CephVersion{})
}

func TestSupported(t *testing.T) {
	for _, v := range supportedVersions {
		assert.True(t, v.Supported())
	}
	for _, v := range unsupportedVersions {
		assert.False(t, v.Supported())
	}
}

func TestIsRelease(t *testing.T) {
	assert.True(t, Luminous.IsRelease(Luminous))
	assert.True(t, Mimic.IsRelease(Mimic))
	assert.True(t, Nautilus.IsRelease(Nautilus))

	assert.False(t, Luminous.IsRelease(Mimic))
	assert.False(t, Luminous.IsRelease(Nautilus))
	assert.False(t, Mimic.IsRelease(Nautilus))

	LuminousUpdate := Luminous
	LuminousUpdate.Minor = 33
	LuminousUpdate.Extra = 4
	assert.True(t, LuminousUpdate.IsRelease(Luminous))

	MimicUpdate := Mimic
	MimicUpdate.Minor = 33
	MimicUpdate.Extra = 4
	assert.True(t, MimicUpdate.IsRelease(Mimic))

	NautilusUpdate := Nautilus
	NautilusUpdate.Minor = 33
	NautilusUpdate.Extra = 4
	assert.True(t, NautilusUpdate.IsRelease(Nautilus))
}

func TestVersionAtLeast(t *testing.T) {
	assert.True(t, Luminous.AtLeast(Luminous))
	assert.False(t, Luminous.AtLeast(Mimic))
	assert.False(t, Luminous.AtLeast(Nautilus))
	assert.True(t, Mimic.AtLeast(Luminous))
	assert.True(t, Mimic.AtLeast(Mimic))
	assert.False(t, Mimic.AtLeast(Nautilus))
	assert.True(t, Nautilus.AtLeast(Luminous))
	assert.True(t, Nautilus.AtLeast(Mimic))
	assert.True(t, Nautilus.AtLeast(Nautilus))
}
