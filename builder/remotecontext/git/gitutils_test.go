package git // import "github.com/docker/docker/builder/remotecontext/git"

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func TestParseRemoteURL(t *testing.T) {
	dir, err := parseRemoteURL("git://github.com/user/repo.git")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gitRepo{"git://github.com/user/repo.git", "master", ""}, dir, cmpGitRepoOpt))

	dir, err = parseRemoteURL("git://github.com/user/repo.git#mybranch:mydir/mysubdir/")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gitRepo{"git://github.com/user/repo.git", "mybranch", "mydir/mysubdir/"}, dir, cmpGitRepoOpt))

	dir, err = parseRemoteURL("https://github.com/user/repo.git")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gitRepo{"https://github.com/user/repo.git", "master", ""}, dir, cmpGitRepoOpt))

	dir, err = parseRemoteURL("https://github.com/user/repo.git#mybranch:mydir/mysubdir/")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gitRepo{"https://github.com/user/repo.git", "mybranch", "mydir/mysubdir/"}, dir, cmpGitRepoOpt))

	dir, err = parseRemoteURL("git@github.com:user/repo.git")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gitRepo{"git@github.com:user/repo.git", "master", ""}, dir, cmpGitRepoOpt))

	dir, err = parseRemoteURL("git@github.com:user/repo.git#mybranch:mydir/mysubdir/")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gitRepo{"git@github.com:user/repo.git", "mybranch", "mydir/mysubdir/"}, dir, cmpGitRepoOpt))
}

var cmpGitRepoOpt = cmp.AllowUnexported(gitRepo{})

func TestCloneArgsSmartHttp(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	serverURL, _ := url.Parse(server.URL)

	serverURL.Path = "/repo.git"

	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("service")
		w.Header().Set("Content-Type", fmt.Sprintf("application/x-%s-advertisement", q))
	})

	args := fetchArgs(serverURL.String(), "master")
	exp := []string{"fetch", "--depth", "1", "origin", "--", "master"}
	assert.Check(t, is.DeepEqual(exp, args))
}

func TestCloneArgsDumbHttp(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	serverURL, _ := url.Parse(server.URL)

	serverURL.Path = "/repo.git"

	mux.HandleFunc("/repo.git/info/refs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
	})

	args := fetchArgs(serverURL.String(), "master")
	exp := []string{"fetch", "origin", "--", "master"}
	assert.Check(t, is.DeepEqual(exp, args))
}

func TestCloneArgsGit(t *testing.T) {
	args := fetchArgs("git://github.com/docker/docker", "master")
	exp := []string{"fetch", "--depth", "1", "origin", "--", "master"}
	assert.Check(t, is.DeepEqual(exp, args))
}

func gitGetConfig(name string) string {
	b, err := git([]string{"config", "--get", name}...)
	if err != nil {
		// since we are interested in empty or non empty string,
		// we can safely ignore the err here.
		return ""
	}
	return strings.TrimSpace(string(b))
}

func TestCheckoutGit(t *testing.T) {
	root, err := ioutil.TempDir("", "docker-build-git-checkout")
	assert.NilError(t, err)
	defer os.RemoveAll(root)

	autocrlf := gitGetConfig("core.autocrlf")
	if !(autocrlf == "true" || autocrlf == "false" ||
		autocrlf == "input" || autocrlf == "") {
		t.Logf("unknown core.autocrlf value: \"%s\"", autocrlf)
	}
	eol := "\n"
	if autocrlf == "true" {
		eol = "\r\n"
	}

	gitDir := filepath.Join(root, "repo")
	_, err = git("init", gitDir)
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "config", "user.email", "test@docker.com")
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "config", "user.name", "Docker test")
	assert.NilError(t, err)

	err = ioutil.WriteFile(filepath.Join(gitDir, "Dockerfile"), []byte("FROM scratch"), 0644)
	assert.NilError(t, err)

	subDir := filepath.Join(gitDir, "subdir")
	assert.NilError(t, os.Mkdir(subDir, 0755))

	err = ioutil.WriteFile(filepath.Join(subDir, "Dockerfile"), []byte("FROM scratch\nEXPOSE 5000"), 0644)
	assert.NilError(t, err)

	if runtime.GOOS != "windows" {
		if err = os.Symlink("../subdir", filepath.Join(gitDir, "parentlink")); err != nil {
			t.Fatal(err)
		}

		if err = os.Symlink("/subdir", filepath.Join(gitDir, "absolutelink")); err != nil {
			t.Fatal(err)
		}
	}

	_, err = gitWithinDir(gitDir, "add", "-A")
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "commit", "-am", "First commit")
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "checkout", "-b", "test")
	assert.NilError(t, err)

	err = ioutil.WriteFile(filepath.Join(gitDir, "Dockerfile"), []byte("FROM scratch\nEXPOSE 3000"), 0644)
	assert.NilError(t, err)

	err = ioutil.WriteFile(filepath.Join(subDir, "Dockerfile"), []byte("FROM busybox\nEXPOSE 5000"), 0644)
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "add", "-A")
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "commit", "-am", "Branch commit")
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "checkout", "master")
	assert.NilError(t, err)

	// set up submodule
	subrepoDir := filepath.Join(root, "subrepo")
	_, err = git("init", subrepoDir)
	assert.NilError(t, err)

	_, err = gitWithinDir(subrepoDir, "config", "user.email", "test@docker.com")
	assert.NilError(t, err)

	_, err = gitWithinDir(subrepoDir, "config", "user.name", "Docker test")
	assert.NilError(t, err)

	err = ioutil.WriteFile(filepath.Join(subrepoDir, "subfile"), []byte("subcontents"), 0644)
	assert.NilError(t, err)

	_, err = gitWithinDir(subrepoDir, "add", "-A")
	assert.NilError(t, err)

	_, err = gitWithinDir(subrepoDir, "commit", "-am", "Subrepo initial")
	assert.NilError(t, err)

	cmd := exec.Command("git", "submodule", "add", subrepoDir, "sub") // this command doesn't work with --work-tree
	cmd.Dir = gitDir
	assert.NilError(t, cmd.Run())

	_, err = gitWithinDir(gitDir, "add", "-A")
	assert.NilError(t, err)

	_, err = gitWithinDir(gitDir, "commit", "-am", "With submodule")
	assert.NilError(t, err)

	type singleCase struct {
		frag      string
		exp       string
		fail      bool
		submodule bool
	}

	cases := []singleCase{
		{"", "FROM scratch", false, true},
		{"master", "FROM scratch", false, true},
		{":subdir", "FROM scratch" + eol + "EXPOSE 5000", false, false},
		{":nosubdir", "", true, false},   // missing directory error
		{":Dockerfile", "", true, false}, // not a directory error
		{"master:nosubdir", "", true, false},
		{"master:subdir", "FROM scratch" + eol + "EXPOSE 5000", false, false},
		{"master:../subdir", "", true, false},
		{"test", "FROM scratch" + eol + "EXPOSE 3000", false, false},
		{"test:", "FROM scratch" + eol + "EXPOSE 3000", false, false},
		{"test:subdir", "FROM busybox" + eol + "EXPOSE 5000", false, false},
	}

	if runtime.GOOS != "windows" {
		// Windows GIT (2.7.1 x64) does not support parentlink/absolutelink. Sample output below
		// 	git --work-tree .\repo --git-dir .\repo\.git add -A
		//	error: readlink("absolutelink"): Function not implemented
		// 	error: unable to index file absolutelink
		// 	fatal: adding files failed
		cases = append(cases, singleCase{frag: "master:absolutelink", exp: "FROM scratch" + eol + "EXPOSE 5000", fail: false})
		cases = append(cases, singleCase{frag: "master:parentlink", exp: "FROM scratch" + eol + "EXPOSE 5000", fail: false})
	}

	for _, c := range cases {
		ref, subdir := getRefAndSubdir(c.frag)
		r, err := cloneGitRepo(gitRepo{remote: gitDir, ref: ref, subdir: subdir})

		if c.fail {
			assert.Check(t, is.ErrorContains(err, ""))
			continue
		}
		assert.NilError(t, err)
		defer os.RemoveAll(r)
		if c.submodule {
			b, err := ioutil.ReadFile(filepath.Join(r, "sub/subfile"))
			assert.NilError(t, err)
			assert.Check(t, is.Equal("subcontents", string(b)))
		} else {
			_, err := os.Stat(filepath.Join(r, "sub/subfile"))
			assert.Assert(t, is.ErrorContains(err, ""))
			assert.Assert(t, os.IsNotExist(err))
		}

		b, err := ioutil.ReadFile(filepath.Join(r, "Dockerfile"))
		assert.NilError(t, err)
		assert.Check(t, is.Equal(c.exp, string(b)))
	}
}

func TestValidGitTransport(t *testing.T) {
	gitUrls := []string{
		"git://github.com/docker/docker",
		"git@github.com:docker/docker.git",
		"git@bitbucket.org:atlassianlabs/atlassian-docker.git",
		"https://github.com/docker/docker.git",
		"http://github.com/docker/docker.git",
		"http://github.com/docker/docker.git#branch",
		"http://github.com/docker/docker.git#:dir",
	}
	incompleteGitUrls := []string{
		"github.com/docker/docker",
	}

	for _, url := range gitUrls {
		if !isGitTransport(url) {
			t.Fatalf("%q should be detected as valid Git prefix", url)
		}
	}

	for _, url := range incompleteGitUrls {
		if isGitTransport(url) {
			t.Fatalf("%q should not be detected as valid Git prefix", url)
		}
	}
}

func TestGitInvalidRef(t *testing.T) {
	gitUrls := []string{
		"git://github.com/moby/moby#--foo bar",
		"git@github.com/moby/moby#--upload-pack=sleep;:",
		"git@g.com:a/b.git#-B",
		"git@g.com:a/b.git#with space",
	}

	for _, url := range gitUrls {
		_, err := Clone(url)
		assert.Assert(t, err != nil)
		assert.Check(t, is.Contains(strings.ToLower(err.Error()), "invalid refspec"))
	}
}
