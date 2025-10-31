package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/cli/build"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

const (
	remoteRepoName = "dockercli/busybox-by-dgst"
	repoName       = privateRegistryURL + "/" + remoteRepoName
)

var (
	pushDigestRegex = regexp.MustCompile(`[\S]+: digest: ([\S]+) size: [0-9]+`)
	digestRegex     = regexp.MustCompile(`Digest: ([\S]+)`)
)

func setupImage(t *testing.T) (digest.Digest, error) {
	return setupImageWithTag(t, "latest")
}

func setupImageWithTag(t *testing.T, tag string) (digest.Digest, error) {
	const containerName = "busyboxbydigest"

	// new file is committed because this layer is used for detecting malicious
	// changes. if this was committed as empty layer it would be skipped on pull
	// and malicious changes would never be detected.
	cli.DockerCmd(t, "run", "-e", "digest=1", "--name", containerName, "busybox", "touch", "anewfile")

	// tag the image to upload it to the private registry
	repoAndTag := repoName + ":" + tag
	cli.DockerCmd(t, "commit", containerName, repoAndTag)

	// delete the container as we don't need it any more
	cli.DockerCmd(t, "rm", "-fv", containerName)

	// push the image
	out := cli.DockerCmd(t, "push", repoAndTag).Combined()

	// delete our local repo that we previously tagged
	cli.DockerCmd(t, "rmi", repoAndTag)

	matches := pushDigestRegex.FindStringSubmatch(out)
	assert.Equal(t, len(matches), 2, "unable to parse digest from push output: %s", out)
	pushDigest := matches[1]

	return digest.Digest(pushDigest), nil
}

func (s *DockerRegistrySuite) TestPullByTagDisplaysDigest(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	pushDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	// pull from the registry using the tag
	out := cli.DockerCmd(t, "pull", repoName).Combined()

	// the pull output includes "Digest: <digest>", so find that
	matches := digestRegex.FindStringSubmatch(out)
	assert.Equal(t, len(matches), 2, "unable to parse digest from push output: %s", out)
	pullDigest := matches[1]

	// make sure the pushed and pull digests match
	assert.Equal(t, pushDigest.String(), pullDigest)
}

func (s *DockerRegistrySuite) TestPullByDigest(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	pushDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	// pull from the registry using the <name>@<digest> reference
	imageReference := fmt.Sprintf("%s@%s", repoName, pushDigest)
	out := cli.DockerCmd(t, "pull", imageReference).Combined()

	// the pull output includes "Digest: <digest>", so find that
	matches := digestRegex.FindStringSubmatch(out)
	assert.Equal(t, len(matches), 2, "unable to parse digest from push output: %s", out)
	pullDigest := matches[1]

	// make sure the pushed and pull digests match
	assert.Equal(t, pushDigest.String(), pullDigest)
}

func (s *DockerRegistrySuite) TestPullByDigestNoFallback(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	// pull from the registry using the <name>@<digest> reference
	imageReference := repoName + "@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	out, _, err := dockerCmdWithError("pull", imageReference)
	assert.Assert(t, err != nil, "expected non-zero exit status and correct error message when pulling non-existing image")

	expectedMsg := fmt.Sprintf("manifest for %s not found", imageReference)
	if testEnv.UsingSnapshotter() {
		expectedMsg = imageReference + ": not found"
	}

	assert.Check(t, is.Contains(out, expectedMsg), "expected non-zero exit status and correct error message when pulling non-existing image")
}

func (s *DockerRegistrySuite) TestCreateByDigest(t *testing.T) {
	pushDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	imageReference := fmt.Sprintf("%s@%s", repoName, pushDigest)

	const containerName = "createByDigest"
	cli.DockerCmd(t, "create", "--name", containerName, imageReference)

	res := inspectField(t, containerName, "Config.Image")
	assert.Equal(t, res, imageReference)
}

func (s *DockerRegistrySuite) TestRunByDigest(t *testing.T) {
	pushDigest, err := setupImage(t)
	assert.NilError(t, err)

	imageReference := fmt.Sprintf("%s@%s", repoName, pushDigest)

	const containerName = "runByDigest"
	out := cli.DockerCmd(t, "run", "--name", containerName, imageReference, "sh", "-c", "echo found=$digest").Combined()

	foundRegex := regexp.MustCompile("found=([^\n]+)")
	matches := foundRegex.FindStringSubmatch(out)
	assert.Equal(t, len(matches), 2, "unable to parse digest from pull output: "+out)
	assert.Equal(t, matches[1], "1", fmt.Sprintf("Expected %q, got %q", "1", matches[1]))

	res := inspectField(t, containerName, "Config.Image")
	assert.Equal(t, res, imageReference)
}

func (s *DockerRegistrySuite) TestRemoveImageByDigest(t *testing.T) {
	imgDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	imageReference := fmt.Sprintf("%s@%s", repoName, imgDigest)

	// pull from the registry using the <name>@<digest> reference
	cli.DockerCmd(t, "pull", imageReference)

	// make sure inspect runs ok
	inspectField(t, imageReference, "Id")

	// do the delete
	err = deleteImages(imageReference)
	assert.NilError(t, err, "unexpected error deleting image")

	// try to inspect again - it should error this time
	_, err = inspectFilter(imageReference, ".Id")
	assert.Assert(t, err != nil, "expected non-zero exit status error deleting image")
	// unexpected nil err trying to inspect what should be a non-existent image
	assert.Assert(t, is.Contains(strings.ToLower(err.Error()), "no such object"))
}

func (s *DockerRegistrySuite) TestBuildByDigest(t *testing.T) {
	imgDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	imageReference := fmt.Sprintf("%s@%s", repoName, imgDigest)

	// pull from the registry using the <name>@<digest> reference
	cli.DockerCmd(t, "pull", imageReference)

	// do the build
	const name = "buildbydigest"
	cli.BuildCmd(t, name, build.WithDockerfile(fmt.Sprintf(
		`FROM %s
     CMD ["/bin/echo", "Hello World"]`, imageReference)))
	assert.NilError(t, err)

	// verify the build was ok
	res := inspectField(t, name, "Config.Cmd")
	assert.Equal(t, res, `[/bin/echo Hello World]`)
}

func (s *DockerRegistrySuite) TestTagByDigest(t *testing.T) {
	imgDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	imageReference := fmt.Sprintf("%s@%s", repoName, imgDigest)

	// pull from the registry using the <name>@<digest> reference
	cli.DockerCmd(t, "pull", imageReference)

	// tag it
	const tag = "tagbydigest"
	cli.DockerCmd(t, "tag", imageReference, tag)

	expectedID := inspectField(t, imageReference, "Id")

	tagID := inspectField(t, tag, "Id")
	assert.Equal(t, tagID, expectedID)
}

func (s *DockerRegistrySuite) TestListImagesWithoutDigests(t *testing.T) {
	imgDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	imageReference := fmt.Sprintf("%s@%s", repoName, imgDigest)

	// pull from the registry using the <name>@<digest> reference
	cli.DockerCmd(t, "pull", imageReference)

	out := cli.DockerCmd(t, "images").Stdout()
	assert.Assert(t, !strings.Contains(out, "DIGEST"), "list output should not have contained DIGEST header")
}

func (s *DockerRegistrySuite) TestListImagesWithDigests(t *testing.T) {
	// setup image1
	digest1, err := setupImageWithTag(t, "tag1")
	assert.NilError(t, err, "error setting up image")
	imageReference1 := fmt.Sprintf("%s@%s", repoName, digest1)
	t.Logf("imageReference1 = %s", imageReference1)

	// pull image1 by digest
	cli.DockerCmd(t, "pull", imageReference1)

	// list images
	out := cli.DockerCmd(t, "images", "--digests").Combined()

	// make sure repo shown, tag=<none>, digest = $digest1
	re1 := regexp.MustCompile(`\s*` + repoName + `\s*<none>\s*` + digest1.String() + `\s`)
	assert.Assert(t, re1.MatchString(out), "expected %q: %s", re1.String(), out)
	// setup image2
	digest2, err := setupImageWithTag(t, "tag2")
	assert.NilError(t, err, "error setting up image")
	imageReference2 := fmt.Sprintf("%s@%s", repoName, digest2)
	t.Logf("imageReference2 = %s", imageReference2)

	// pull image1 by digest
	cli.DockerCmd(t, "pull", imageReference1)

	// pull image2 by digest
	cli.DockerCmd(t, "pull", imageReference2)

	// list images
	out = cli.DockerCmd(t, "images", "--digests").Stdout()

	// make sure repo shown, tag=<none>, digest = $digest1
	assert.Assert(t, re1.MatchString(out), "expected %q: %s", re1.String(), out)

	// make sure repo shown, tag=<none>, digest = $digest2
	re2 := regexp.MustCompile(`\s*` + repoName + `\s*<none>\s*` + digest2.String() + `\s`)
	assert.Assert(t, re2.MatchString(out), "expected %q: %s", re2.String(), out)

	// pull tag1
	cli.DockerCmd(t, "pull", repoName+":tag1")

	// list images
	out = cli.DockerCmd(t, "images", "--digests").Stdout()

	// make sure image 1 has repo, tag, <none> AND repo, <none>, digest
	reWithDigest1 := regexp.MustCompile(`\s*` + repoName + `\s*tag1\s*` + digest1.String() + `\s`)
	assert.Assert(t, reWithDigest1.MatchString(out), "expected %q: %s", reWithDigest1.String(), out)
	// make sure image 2 has repo, <none>, digest
	assert.Assert(t, re2.MatchString(out), "expected %q: %s", re2.String(), out)

	// pull tag 2
	cli.DockerCmd(t, "pull", repoName+":tag2")

	// list images
	out = cli.DockerCmd(t, "images", "--digests").Stdout()

	// make sure image 1 has repo, tag, digest
	assert.Assert(t, reWithDigest1.MatchString(out), "expected %q: %s", reWithDigest1.String(), out)

	// make sure image 2 has repo, tag, digest
	reWithDigest2 := regexp.MustCompile(`\s*` + repoName + `\s*tag2\s*` + digest2.String() + `\s`)
	assert.Assert(t, reWithDigest2.MatchString(out), "expected %q: %s", reWithDigest2.String(), out)

	// list images
	out = cli.DockerCmd(t, "images", "--digests").Stdout()

	// make sure image 1 has repo, tag, digest
	assert.Assert(t, reWithDigest1.MatchString(out), "expected %q: %s", reWithDigest1.String(), out)
	// make sure image 2 has repo, tag, digest
	assert.Assert(t, reWithDigest2.MatchString(out), "expected %q: %s", reWithDigest2.String(), out)
	// We always have a digest when using containerd to store images
	if !testEnv.UsingSnapshotter() {
		// make sure busybox has tag, but not digest
		busyboxRe := regexp.MustCompile(`\s*busybox\s*latest\s*<none>\s`)
		assert.Assert(t, busyboxRe.MatchString(out), "expected %q: %s", busyboxRe.String(), out)
	}
}

func (s *DockerRegistrySuite) TestListDanglingImagesWithDigests(t *testing.T) {
	// See https://github.com/moby/moby/pull/46856
	skip.If(t, testEnv.UsingSnapshotter(), "dangling=true filter behaves a bit differently with c8d")

	// setup image1
	digest1, err := setupImageWithTag(t, "dangle1")
	assert.NilError(t, err, "error setting up image")
	imageReference1 := fmt.Sprintf("%s@%s", repoName, digest1)
	t.Logf("imageReference1 = %s", imageReference1)

	// pull image1 by digest
	cli.DockerCmd(t, "pull", imageReference1)

	// list images
	out := cli.DockerCmd(t, "images", "--digests").Stdout()

	// make sure repo shown, tag=<none>, digest = $digest1
	re1 := regexp.MustCompile(`\s*` + repoName + `\s*<none>\s*` + digest1.String() + `\s`)
	assert.Assert(t, re1.MatchString(out), "expected %q: %s", re1.String(), out)
	// setup image2
	digest2, err := setupImageWithTag(t, "dangle2")
	// error setting up image
	assert.NilError(t, err)
	imageReference2 := fmt.Sprintf("%s@%s", repoName, digest2)
	t.Logf("imageReference2 = %s", imageReference2)

	// pull image1 by digest
	cli.DockerCmd(t, "pull", imageReference1)

	// pull image2 by digest
	cli.DockerCmd(t, "pull", imageReference2)

	// list images
	out = cli.DockerCmd(t, "images", "--digests", "--filter=dangling=true").Stdout()

	// make sure repo shown, tag=<none>, digest = $digest1
	assert.Assert(t, re1.MatchString(out), "expected %q: %s", re1.String(), out)

	// make sure repo shown, tag=<none>, digest = $digest2
	re2 := regexp.MustCompile(`\s*` + repoName + `\s*<none>\s*` + digest2.String() + `\s`)
	assert.Assert(t, re2.MatchString(out), "expected %q: %s", re2.String(), out)

	// pull dangle1 tag
	cli.DockerCmd(t, "pull", repoName+":dangle1")

	// list images
	out = cli.DockerCmd(t, "images", "--digests", "--filter=dangling=true").Stdout()

	// make sure image 1 has repo, tag, <none> AND repo, <none>, digest
	reWithDigest1 := regexp.MustCompile(`\s*` + repoName + `\s*dangle1\s*` + digest1.String() + `\s`)
	assert.Assert(t, !reWithDigest1.MatchString(out), "unexpected %q: %s", reWithDigest1.String(), out)
	// make sure image 2 has repo, <none>, digest
	assert.Assert(t, re2.MatchString(out), "expected %q: %s", re2.String(), out)

	// pull dangle2 tag
	cli.DockerCmd(t, "pull", repoName+":dangle2")

	// list images, show tagged images
	out = cli.DockerCmd(t, "images", "--digests").Stdout()

	// make sure image 1 has repo, tag, digest
	assert.Assert(t, reWithDigest1.MatchString(out), "expected %q: %s", reWithDigest1.String(), out)

	// make sure image 2 has repo, tag, digest
	reWithDigest2 := regexp.MustCompile(`\s*` + repoName + `\s*dangle2\s*` + digest2.String() + `\s`)
	assert.Assert(t, reWithDigest2.MatchString(out), "expected %q: %s", reWithDigest2.String(), out)

	// list images, no longer dangling, should not match
	out = cli.DockerCmd(t, "images", "--digests", "--filter=dangling=true").Stdout()

	// make sure image 1 has repo, tag, digest
	assert.Assert(t, !reWithDigest1.MatchString(out), "unexpected %q: %s", reWithDigest1.String(), out)
	// make sure image 2 has repo, tag, digest
	assert.Assert(t, !reWithDigest2.MatchString(out), "unexpected %q: %s", reWithDigest2.String(), out)
}

func (s *DockerRegistrySuite) TestInspectImageWithDigests(t *testing.T) {
	imgDigest, err := setupImage(t)
	assert.Assert(t, err == nil, "error setting up image")

	imageReference := fmt.Sprintf("%s@%s", repoName, imgDigest)

	// pull from the registry using the <name>@<digest> reference
	cli.DockerCmd(t, "pull", imageReference)

	out := cli.DockerCmd(t, "inspect", imageReference).Stdout()

	var imageJSON []image.InspectResponse
	err = json.Unmarshal([]byte(out), &imageJSON)
	assert.NilError(t, err)
	assert.Equal(t, len(imageJSON), 1)
	assert.Equal(t, len(imageJSON[0].RepoDigests), 1)
	assert.Check(t, is.Contains(imageJSON[0].RepoDigests, imageReference))
}

func (s *DockerRegistrySuite) TestPsListContainersFilterAncestorImageByDigest(t *testing.T) {
	existingContainers := ExistingContainerIDs(t)

	imgDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	imageReference := fmt.Sprintf("%s@%s", repoName, imgDigest)

	// pull from the registry using the <name>@<digest> reference
	cli.DockerCmd(t, "pull", imageReference)

	// build an image from it
	const imageName1 = "images_ps_filter_test"
	cli.BuildCmd(t, imageName1, build.WithDockerfile(fmt.Sprintf(
		`FROM %s
		 LABEL match me 1`, imageReference)),
		build.WithBuildkit(false), // FIXME(thaJeztah): rewrite test to have something more predictable
	)

	// run a container based on that
	cli.DockerCmd(t, "run", "--name=test1", imageReference, "echo", "hello")
	expectedID := getIDByName(t, "test1")

	// run a container based on the a descendant of that too
	cli.DockerCmd(t, "run", "--name=test2", imageName1, "echo", "hello")
	expectedID1 := getIDByName(t, "test2")

	expectedIDs := []string{expectedID, expectedID1}

	// Invalid imageReference
	out := cli.DockerCmd(t, "ps", "-a", "-q", "--no-trunc", fmt.Sprintf("--filter=ancestor=busybox@%s", imgDigest)).Stdout()
	assert.Equal(t, strings.TrimSpace(out), "", "Filter container for ancestor filter should be empty")

	// Valid imageReference
	out = cli.DockerCmd(t, "ps", "-a", "-q", "--no-trunc", "--filter=ancestor="+imageReference).Stdout()
	checkPsAncestorFilterOutput(t, RemoveOutputForExistingElements(out, existingContainers), imageReference, expectedIDs)
}

func (s *DockerRegistrySuite) TestDeleteImageByIDOnlyPulledByDigest(t *testing.T) {
	pushDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	// pull from the registry using the <name>@<digest> reference
	imageReference := fmt.Sprintf("%s@%s", repoName, pushDigest)
	cli.DockerCmd(t, "pull", imageReference)
	// just in case...

	cli.DockerCmd(t, "tag", imageReference, repoName+":sometag")

	imageID := inspectField(t, imageReference, "Id")

	cli.DockerCmd(t, "rmi", imageID)

	_, err = inspectFilter(imageID, ".Id")
	assert.ErrorContains(t, err, "", "image should have been deleted")
}

func (s *DockerRegistrySuite) TestDeleteImageWithDigestAndTag(t *testing.T) {
	pushDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	// pull from the registry using the <name>@<digest> reference
	imageReference := fmt.Sprintf("%s@%s", repoName, pushDigest)
	cli.DockerCmd(t, "pull", imageReference)

	imageID := inspectField(t, imageReference, "Id")

	const repoTag = repoName + ":sometag"
	const repoTag2 = repoName + ":othertag"
	cli.DockerCmd(t, "tag", imageReference, repoTag)
	cli.DockerCmd(t, "tag", imageReference, repoTag2)

	cli.DockerCmd(t, "rmi", repoTag2)

	// rmi should have deleted only repoTag2, because there's another tag
	inspectField(t, repoTag, "Id")

	cli.DockerCmd(t, "rmi", repoTag)

	// rmi should have deleted the tag, the digest reference, and the image itself
	_, err = inspectFilter(imageID, ".Id")
	assert.ErrorContains(t, err, "", "image should have been deleted")
}

func (s *DockerRegistrySuite) TestDeleteImageWithDigestAndMultiRepoTag(t *testing.T) {
	pushDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	repo2 := fmt.Sprintf("%s/%s", repoName, "repo2")

	// pull from the registry using the <name>@<digest> reference
	imageReference := fmt.Sprintf("%s@%s", repoName, pushDigest)
	cli.DockerCmd(t, "pull", imageReference)

	imageID := inspectField(t, imageReference, "Id")

	repoTag := repoName + ":sometag"
	repoTag2 := repo2 + ":othertag"
	cli.DockerCmd(t, "tag", imageReference, repoTag)
	cli.DockerCmd(t, "tag", imageReference, repoTag2)

	cli.DockerCmd(t, "rmi", repoTag)

	// rmi should have deleted repoTag and image reference, but left repoTag2
	inspectField(t, repoTag2, "Id")
	_, err = inspectFilter(imageReference, ".Id")
	assert.ErrorContains(t, err, "", "image digest reference should have been removed")

	_, err = inspectFilter(repoTag, ".Id")
	assert.ErrorContains(t, err, "", "image tag reference should have been removed")

	cli.DockerCmd(t, "rmi", repoTag2)

	// rmi should have deleted the tag, the digest reference, and the image itself
	_, err = inspectFilter(imageID, ".Id")
	assert.ErrorContains(t, err, "", "image should have been deleted")
}

// TestPullFailsWithAlteredManifest tests that a `docker pull` fails when
// we have modified a manifest blob and its digest cannot be verified.
// This is the schema2 version of the test.
func (s *DockerRegistrySuite) TestPullFailsWithAlteredManifest(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	manifestDigest, err := setupImage(t)
	assert.NilError(t, err, "error setting up image")

	// Load the target manifest blob.
	manifestBlob := s.reg.ReadBlobContents(t, manifestDigest)

	var imgManifest ocispec.Manifest
	err = json.Unmarshal(manifestBlob, &imgManifest)
	assert.NilError(t, err, "unable to decode image manifest from blob")

	// Change a layer in the manifest.
	imgManifest.Layers[0].Digest = digest.Digest("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	// Move the existing data file aside, so that we can replace it with a
	// malicious blob of data. NOTE: we defer the returned undo func.
	undo := s.reg.TempMoveBlobData(t, manifestDigest)
	defer undo()

	alteredManifestBlob, err := json.MarshalIndent(imgManifest, "", "   ")
	assert.NilError(t, err, "unable to encode altered image manifest to JSON")

	s.reg.WriteBlobContents(t, manifestDigest, alteredManifestBlob)

	// Now try pulling that image by digest. We should get an error about
	// digest verification for the manifest digest.

	// Pull from the registry using the <name>@<digest> reference.
	imageReference := fmt.Sprintf("%s@%s", repoName, manifestDigest)
	out, exitStatus, _ := dockerCmdWithError("pull", imageReference)
	assert.Assert(t, exitStatus != 0)

	if testEnv.UsingSnapshotter() {
		assert.Assert(t, is.Contains(out, "unexpected commit digest"))
		assert.Assert(t, is.Contains(out, "expected "+manifestDigest))
	} else {
		assert.Assert(t, is.Contains(out, fmt.Sprintf("manifest verification failed for digest %s", manifestDigest)))
	}
}

// TestPullFailsWithAlteredLayer tests that a `docker pull` fails when
// we have modified a layer blob and its digest cannot be verified.
// This is the schema2 version of the test.
func (s *DockerRegistrySuite) TestPullFailsWithAlteredLayer(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	skip.If(t, testEnv.UsingSnapshotter(), "Faked layer is already in the content store, so it won't be fetched from the repository at all.")

	manifestDigest, err := setupImage(t)
	assert.NilError(t, err)

	// Load the target manifest blob.
	manifestBlob := s.reg.ReadBlobContents(t, manifestDigest)

	var imgManifest ocispec.Manifest
	err = json.Unmarshal(manifestBlob, &imgManifest)
	assert.NilError(t, err)

	// Next, get the digest of one of the layers from the manifest.
	targetLayerDigest := imgManifest.Layers[0].Digest

	// Move the existing data file aside, so that we can replace it with a
	// malicious blob of data. NOTE: we defer the returned undo func.
	undo := s.reg.TempMoveBlobData(t, targetLayerDigest)
	defer undo()

	// Now make a fake data blob in this directory.
	s.reg.WriteBlobContents(t, targetLayerDigest, []byte("This is not the data you are looking for."))

	// Now try pulling that image by digest. We should get an error about
	// digest verification for the target layer digest.

	// Remove distribution cache to force a re-pull of the blobs
	if err := os.RemoveAll(filepath.Join(testEnv.DaemonInfo.DockerRootDir, "image", s.d.StorageDriver(), "distribution")); err != nil {
		t.Fatalf("error clearing distribution cache: %v", err)
	}

	// Pull from the registry using the <name>@<digest> reference.
	imageReference := fmt.Sprintf("%s@%s", repoName, manifestDigest)
	out, exitStatus, _ := dockerCmdWithError("pull", imageReference)
	assert.Assert(t, exitStatus != 0, "expected a non-zero exit status")

	expectedErrorMsg := fmt.Sprintf("filesystem layer verification failed for digest %s", targetLayerDigest)
	assert.Assert(t, strings.Contains(out, expectedErrorMsg), "expected error message in output: %s", out)
}
