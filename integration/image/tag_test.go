package image // import "github.com/docker/docker/integration/image"

import (
	"context"
	"fmt"
	"testing"

	"github.com/docker/docker/testutil"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/skip"
)

// tagging a named image in a new unprefixed repo should work
func TestTagUnprefixedRepoByNameOrName(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	// By name
	err := client.ImageTag(ctx, "busybox:latest", "testfoobarbaz")
	assert.NilError(t, err)

	// By ID
	insp, _, err := client.ImageInspectWithRaw(ctx, "busybox")
	assert.NilError(t, err)
	err = client.ImageTag(ctx, insp.ID, "testfoobarbaz")
	assert.NilError(t, err)
}

// ensure we don't allow the use of invalid repository names or tags; these tag operations should fail
// TODO (yongtang): Migrate to unit tests
func TestTagInvalidReference(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	invalidRepos := []string{"fo$z$", "Foo@3cc", "Foo$3", "Foo*3", "Fo^3", "Foo!3", "F)xcz(", "fo%asd", "FOO/bar"}

	for _, repo := range invalidRepos {
		err := client.ImageTag(ctx, "busybox", repo)
		assert.Check(t, is.ErrorContains(err, "not a valid repository/tag"))
	}

	longTag := testutil.GenerateRandomAlphaOnlyString(121)

	invalidTags := []string{"repo:fo$z$", "repo:Foo@3cc", "repo:Foo$3", "repo:Foo*3", "repo:Fo^3", "repo:Foo!3", "repo:%goodbye", "repo:#hashtagit", "repo:F)xcz(", "repo:-foo", "repo:..", longTag}

	for _, repotag := range invalidTags {
		err := client.ImageTag(ctx, "busybox", repotag)
		assert.Check(t, is.ErrorContains(err, "not a valid repository/tag"))
	}

	// test repository name begin with '-'
	err := client.ImageTag(ctx, "busybox:latest", "-busybox:test")
	assert.Check(t, is.ErrorContains(err, "Error parsing reference"))

	// test namespace name begin with '-'
	err = client.ImageTag(ctx, "busybox:latest", "-test/busybox:test")
	assert.Check(t, is.ErrorContains(err, "Error parsing reference"))

	// test index name begin with '-'
	err = client.ImageTag(ctx, "busybox:latest", "-index:5000/busybox:test")
	assert.Check(t, is.ErrorContains(err, "Error parsing reference"))

	// test setting tag fails
	err = client.ImageTag(ctx, "busybox:latest", "sha256:sometag")
	assert.Check(t, is.ErrorContains(err, "refusing to create an ambiguous tag using digest algorithm as name"))
}

// ensure we allow the use of valid tags
func TestTagValidPrefixedRepo(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	validRepos := []string{"fooo/bar", "fooaa/test", "foooo:t", "HOSTNAME.DOMAIN.COM:443/foo/bar"}

	for _, repo := range validRepos {
		err := client.ImageTag(ctx, "busybox", repo)
		assert.NilError(t, err)
	}
}

// tag an image with an existed tag name without -f option should work
func TestTagExistedNameWithoutForce(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	err := client.ImageTag(ctx, "busybox:latest", "busybox:test")
	assert.NilError(t, err)
}

// ensure tagging using official names works
// ensure all tags result in the same name
func TestTagOfficialNames(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	names := []string{
		"docker.io/busybox",
		"index.docker.io/busybox",
		"library/busybox",
		"docker.io/library/busybox",
		"index.docker.io/library/busybox",
	}

	for _, name := range names {
		err := client.ImageTag(ctx, "busybox", name+":latest")
		assert.NilError(t, err)

		// ensure we don't have multiple tag names.
		insp, _, err := client.ImageInspectWithRaw(ctx, "busybox")
		assert.NilError(t, err)
		assert.Assert(t, !is.Contains(insp.RepoTags, name)().Success())
	}

	for _, name := range names {
		err := client.ImageTag(ctx, name+":latest", "fooo/bar:latest")
		assert.NilError(t, err)
	}
}

// ensure tags can not match digests
func TestTagMatchesDigest(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()
	ctx := context.Background()

	digest := "busybox@sha256:abcdef76720241213f5303bda7704ec4c2ef75613173910a56fb1b6e20251507"
	// test setting tag fails
	err := client.ImageTag(ctx, "busybox:latest", digest)
	assert.Check(t, is.ErrorContains(err, "refusing to create a tag with a digest reference"))

	// check that no new image matches the digest
	_, _, err = client.ImageInspectWithRaw(ctx, digest)
	assert.Check(t, is.ErrorContains(err, fmt.Sprintf("No such image: %s", digest)))
}
