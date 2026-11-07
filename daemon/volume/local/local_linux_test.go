//go:build linux

package local

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/daemon/internal/idtools"
	"github.com/moby/moby/v2/daemon/internal/quota"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const (
	quotaSize        = 1024 * 1024
	quotaSizeLiteral = "1M"
)

func TestQuota(t *testing.T) {
	// Check if the test can be run
	quota.RequireSupported(t)

	// get sparse xfs test image
	imageFileName, err := quota.PrepareQuotaTestImage(t)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(imageFileName)

	t.Run("testVolWithQuota", quota.WrapMountTest(imageFileName, true, testVolWithQuota))
	t.Run("testVolQuotaUnsupported", quota.WrapMountTest(imageFileName, false, testVolQuotaUnsupported))
}

func testVolWithQuota(t *testing.T, mountPoint, backingFsDev, testDir string) {
	r, err := New(testDir, idtools.Identity{UID: os.Geteuid(), GID: os.Getegid()})
	if err != nil {
		t.Fatal(err)
	}
	assert.Assert(t, r.quotaCtl != nil)

	vol, err := r.Create("testing", map[string]string{"size": quotaSizeLiteral})
	if err != nil {
		t.Fatal(err)
	}

	dir, err := vol.Mount("1234")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := vol.Unmount("1234"); err != nil {
			t.Fatal(err)
		}
	}()

	testfile := filepath.Join(dir, "testfile")

	// test writing file smaller than quota
	assert.NilError(t, os.WriteFile(testfile, make([]byte, quotaSize/2), 0o644))
	assert.NilError(t, os.Remove(testfile))

	// test writing file larger than quota
	err = os.WriteFile(testfile, make([]byte, quotaSize+1), 0o644)
	assert.ErrorContains(t, err, "")
	if _, err := os.Stat(testfile); err == nil {
		assert.NilError(t, os.Remove(testfile))
	}
}

func testVolQuotaUnsupported(t *testing.T, mountPoint, backingFsDev, testDir string) {
	r, err := New(testDir, idtools.Identity{UID: os.Geteuid(), GID: os.Getegid()})
	if err != nil {
		t.Fatal(err)
	}
	assert.Assert(t, is.Nil(r.quotaCtl))

	_, err = r.Create("testing", map[string]string{"size": quotaSizeLiteral})
	assert.ErrorContains(t, err, "no quota support")

	vol, err := r.Create("testing", nil)
	if err != nil {
		t.Fatal(err)
	}

	// this could happen if someone moves volumes from storage with
	// quota support to some place without
	lv, ok := vol.(*localVolume)
	assert.Assert(t, ok)
	lv.opts = &optsConfig{
		Quota: quota.Quota{Size: quotaSize},
	}

	_, err = vol.Mount("1234")
	assert.ErrorContains(t, err, "no quota support")
}

func TestVolCreateValidation(t *testing.T) {
	r, err := New(t.TempDir(), idtools.Identity{UID: os.Geteuid(), GID: os.Getegid()})
	if err != nil {
		t.Fatal(err)
	}

	mandatoryOpts = map[string][]string{
		"device": {"type"},
		"type":   {"device"},
		"o":      {"device", "type"},
	}

	tests := []struct {
		doc         string
		name        string
		opts        map[string]string
		expectedErr string
	}{
		{
			doc:  "invalid: name too short",
			name: "a",
			opts: map[string]string{
				"type":   "foo",
				"device": "foo",
			},
			expectedErr: `volume name is too short, names should be at least two alphanumeric characters`,
		},
		{
			doc:  "invalid: name invalid characters",
			name: "hello world",
			opts: map[string]string{
				"type":   "foo",
				"device": "foo",
			},
			expectedErr: `"hello world" includes invalid characters for a local volume name, only "[a-zA-Z0-9][a-zA-Z0-9_.-]" are allowed. If you intended to pass a host directory, use absolute path`,
		},
		{
			doc:         "invalid: unknown option",
			opts:        map[string]string{"hello": "world"},
			expectedErr: `invalid option: "hello"`,
		},
		{
			doc:         "invalid: invalid size",
			opts:        map[string]string{"size": "hello"},
			expectedErr: `invalid size: 'hello'`,
		},
		{
			doc:         "invalid: size, but no quotactl",
			opts:        map[string]string{"size": "1234"},
			expectedErr: `quota size requested but no quota support`,
		},
		{
			doc: "invalid: device without type",
			opts: map[string]string{
				"device": "foo",
			},
			expectedErr: `missing required option: "type"`,
		},
		{
			doc: "invalid: type without device",
			opts: map[string]string{
				"type": "foo",
			},
			expectedErr: `missing required option: "device"`,
		},
		{
			doc: "invalid: o without device",
			opts: map[string]string{
				"o":    "foo",
				"type": "foo",
			},
			expectedErr: `missing required option: "device"`,
		},
		{
			doc: "invalid: o without type",
			opts: map[string]string{
				"o":      "foo",
				"device": "foo",
			},
			expectedErr: `missing required option: "type"`,
		},
		{
			doc:  "valid: short name, no options",
			name: "ab",
		},
		{
			doc: "valid: device and type",
			opts: map[string]string{
				"type":   "foo",
				"device": "foo",
			},
		},
		{
			doc: "valid: device, type, and o",
			opts: map[string]string{
				"type":   "foo",
				"device": "foo",
				"o":      "foo",
			},
		},
		{
			doc: "cifs",
			opts: map[string]string{
				"type":   "cifs",
				"device": "//some.example.com/thepath",
				"o":      "foo",
			},
		},
		{
			doc: "cifs with port in url",
			opts: map[string]string{
				"type":   "cifs",
				"device": "//some.example.com:2345/thepath",
				"o":      "foo",
			},
			expectedErr: "port not allowed in CIFS device URL, include 'port' in 'o='",
		},
		{
			doc: "cifs with bad url",
			opts: map[string]string{
				"type":   "cifs",
				"device": ":::",
				"o":      "foo",
			},
			expectedErr: `error parsing mount device url: parse ":::": missing protocol scheme`,
		},
	}

	for i, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			if tc.name == "" {
				tc.name = "vol-" + strconv.Itoa(i)
			}
			v, err := r.Create(tc.name, tc.opts)
			if v != nil {
				defer func() {
					assert.Check(t, r.Remove(v))
				}()
			}
			if tc.expectedErr == "" {
				assert.NilError(t, err)
			} else {
				assert.Check(t, cerrdefs.IsInvalidArgument(err), "got: %T", err)
				assert.Check(t, is.ErrorContains(err, tc.expectedErr))
			}
		})
	}
}

func TestVolMountOpts(t *testing.T) {
	tests := []struct {
		name                         string
		opts                         optsConfig
		expectedErr                  string
		expectedDevice, expectedOpts string
	}{
		{
			name: "cifs url with space",
			opts: optsConfig{
				MountType:   "cifs",
				MountDevice: "//1.2.3.4/Program Files",
			},
			expectedDevice: "//1.2.3.4/Program Files",
			expectedOpts:   "",
		},
		{
			name: "cifs resolve addr",
			opts: optsConfig{
				MountType:   "cifs",
				MountDevice: "//example.com/Program Files",
				MountOpts:   "addr=example.com",
			},
			expectedDevice: "//example.com/Program Files",
			expectedOpts:   "addr=1.2.3.4",
		},
		{
			name: "cifs resolve device",
			opts: optsConfig{
				MountType:   "cifs",
				MountDevice: "//example.com/Program Files",
			},
			expectedDevice: "//1.2.3.4/Program Files",
		},
		{
			name: "nfs dont resolve device",
			opts: optsConfig{
				MountType:   "nfs",
				MountDevice: "//example.com/Program Files",
			},
			expectedDevice: "//example.com/Program Files",
		},
		{
			name: "nfs resolve addr",
			opts: optsConfig{
				MountType:   "nfs",
				MountDevice: "//example.com/Program Files",
				MountOpts:   "addr=example.com",
			},
			expectedDevice: "//example.com/Program Files",
			expectedOpts:   "addr=1.2.3.4",
		},
	}

	ip1234 := net.ParseIP("1.2.3.4")
	resolveIP := func(network, addr string) (*net.IPAddr, error) {
		switch addr {
		case "example.com":
			return &net.IPAddr{IP: ip1234}, nil
		}

		return nil, &net.DNSError{Err: "no such host", Name: addr, IsNotFound: true}
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dev, opts, err := getMountOptions(&tc.opts, resolveIP)

			if tc.expectedErr != "" {
				assert.Check(t, is.ErrorContains(err, tc.expectedErr))
			} else {
				assert.Check(t, err)
			}

			assert.Check(t, is.Equal(dev, tc.expectedDevice))
			assert.Check(t, is.Equal(opts, tc.expectedOpts))
		})
	}
}
