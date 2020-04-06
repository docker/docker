package registry // import "github.com/docker/docker/testutil/registry"

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/docker/docker/testutil"
	"github.com/opencontainers/go-digest"
	"gotest.tools/assert"
)

const (
	// V2binary is the name of the registry v2 binary
	V2binary = "registry-v2"
	// V2binarySchema1 is the name of the registry that serve schema1
	V2binarySchema1 = "registry-v2-schema1"
	// DefaultURL is the default url that will be used by the registry (if not specified otherwise)
	DefaultURL = "127.0.0.1:5000"
)

type testingT interface {
	assert.TestingT
	logT
	Fatal(...interface{})
	Fatalf(string, ...interface{})
}

type logT interface {
	Logf(string, ...interface{})
}

// V2 represent a registry version 2
type V2 struct {
	cmd         *exec.Cmd
	registryURL string
	dir         string
	auth        string
	username    string
	password    string
	email       string
}

// Config contains the test registry configuration
type Config struct {
	schema1     bool
	auth        string
	tokenURL    string
	registryURL string
}

// NewV2 creates a v2 registry server
func NewV2(t testingT, ops ...func(*Config)) *V2 {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	c := &Config{
		registryURL: DefaultURL,
	}
	for _, op := range ops {
		op(c)
	}
	tmp, err := ioutil.TempDir("", "registry-test-")
	assert.NilError(t, err)
	template := `version: 0.1
loglevel: debug
storage:
    filesystem:
        rootdirectory: %s
http:
    addr: %s
%s`
	var (
		authTemplate string
		username     string
		password     string
		email        string
	)
	switch c.auth {
	case "htpasswd":
		htpasswdPath := filepath.Join(tmp, "htpasswd")
		// generated with: htpasswd -Bbn testuser testpassword
		userpasswd := "testuser:$2y$05$sBsSqk0OpSD1uTZkHXc4FeJ0Z70wLQdAX/82UiHuQOKbNbBrzs63m"
		username = "testuser"
		password = "testpassword"
		email = "test@test.org"
		err := ioutil.WriteFile(htpasswdPath, []byte(userpasswd), os.FileMode(0644))
		assert.NilError(t, err)
		authTemplate = fmt.Sprintf(`auth:
    htpasswd:
        realm: basic-realm
        path: %s
`, htpasswdPath)
	case "token":
		authTemplate = fmt.Sprintf(`auth:
    token:
        realm: %s
        service: "registry"
        issuer: "auth-registry"
        rootcertbundle: "fixtures/registry/cert.pem"
`, c.tokenURL)
	}

	confPath := filepath.Join(tmp, "config.yaml")
	config, err := os.Create(confPath)
	assert.NilError(t, err)
	defer config.Close()

	if _, err := fmt.Fprintf(config, template, tmp, c.registryURL, authTemplate); err != nil {
		// FIXME(vdemeester) use a defer/clean func
		os.RemoveAll(tmp)
		t.Fatal(err)
	}

	binary := V2binary
	if c.schema1 {
		binary = V2binarySchema1
	}
	cmd := exec.Command(binary, confPath)
	if err := cmd.Start(); err != nil {
		// FIXME(vdemeester) use a defer/clean func
		os.RemoveAll(tmp)
		t.Fatal(err)
	}
	return &V2{
		cmd:         cmd,
		dir:         tmp,
		auth:        c.auth,
		username:    username,
		password:    password,
		email:       email,
		registryURL: c.registryURL,
	}
}

// WaitReady waits for the registry to be ready to serve requests (or fail after a while)
func (r *V2) WaitReady(t testingT) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	var err error
	for i := 0; i != 50; i++ {
		if err = r.Ping(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for test registry to become available: %v", err)
}

// Ping sends an http request to the current registry, and fail if it doesn't respond correctly
func (r *V2) Ping() error {
	// We always ping through HTTP for our test registry.
	resp, err := http.Get(fmt.Sprintf("http://%s/v2/", r.registryURL))
	if err != nil {
		return err
	}
	resp.Body.Close()

	fail := resp.StatusCode != http.StatusOK
	if r.auth != "" {
		// unauthorized is a _good_ status when pinging v2/ and it needs auth
		fail = fail && resp.StatusCode != http.StatusUnauthorized
	}
	if fail {
		return fmt.Errorf("registry ping replied with an unexpected status code %d", resp.StatusCode)
	}
	return nil
}

// Close kills the registry server
func (r *V2) Close() {
	r.cmd.Process.Kill()
	r.cmd.Process.Wait()
	os.RemoveAll(r.dir)
}

func (r *V2) getBlobFilename(blobDigest digest.Digest) string {
	// Split the digest into its algorithm and hex components.
	dgstAlg, dgstHex := blobDigest.Algorithm(), blobDigest.Hex()

	// The path to the target blob data looks something like:
	//   baseDir + "docker/registry/v2/blobs/sha256/a3/a3ed...46d4/data"
	return fmt.Sprintf("%s/docker/registry/v2/blobs/%s/%s/%s/data", r.dir, dgstAlg, dgstHex[:2], dgstHex)
}

// ReadBlobContents read the file corresponding to the specified digest
func (r *V2) ReadBlobContents(t assert.TestingT, blobDigest digest.Digest) []byte {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	// Load the target manifest blob.
	manifestBlob, err := ioutil.ReadFile(r.getBlobFilename(blobDigest))
	assert.NilError(t, err, "unable to read blob")
	return manifestBlob
}

// WriteBlobContents write the file corresponding to the specified digest with the given content
func (r *V2) WriteBlobContents(t assert.TestingT, blobDigest digest.Digest, data []byte) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	err := ioutil.WriteFile(r.getBlobFilename(blobDigest), data, os.FileMode(0644))
	assert.NilError(t, err, "unable to write malicious data blob")
}

// TempMoveBlobData moves the existing data file aside, so that we can replace it with a
// malicious blob of data for example.
func (r *V2) TempMoveBlobData(t testingT, blobDigest digest.Digest) (undo func()) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	tempFile, err := ioutil.TempFile("", "registry-temp-blob-")
	assert.NilError(t, err, "unable to get temporary blob file")
	tempFile.Close()

	blobFilename := r.getBlobFilename(blobDigest)

	// Move the existing data file aside, so that we can replace it with a
	// another blob of data.
	if err := os.Rename(blobFilename, tempFile.Name()); err != nil {
		// FIXME(vdemeester) use a defer/clean func
		os.Remove(tempFile.Name())
		t.Fatalf("unable to move data blob: %s", err)
	}

	return func() {
		os.Rename(tempFile.Name(), blobFilename)
		os.Remove(tempFile.Name())
	}
}

// Username returns the configured user name of the server
func (r *V2) Username() string {
	return r.username
}

// Password returns the configured password of the server
func (r *V2) Password() string {
	return r.password
}

// Email returns the configured email of the server
func (r *V2) Email() string {
	return r.email
}

// Path returns the path where the registry write data
func (r *V2) Path() string {
	return filepath.Join(r.dir, "docker", "registry", "v2")
}
