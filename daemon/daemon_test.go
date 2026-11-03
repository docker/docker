package daemon

import (
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/idtools"
	"github.com/moby/moby/v2/daemon/libnetwork"
	volumesservice "github.com/moby/moby/v2/daemon/volume/service"
	"github.com/pkg/errors"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

//
// https://github.com/moby/moby/issues/8069
//

func TestGetContainer(t *testing.T) {
	c1 := &container.Container{
		ID:   "5a4ff6a163ad4533d22d69a2b8960bf7fafdcba06e72d2febdba229008b0bf57",
		Name: "tender_bardeen",
	}

	c2 := &container.Container{
		ID:   "3cdbd1aa394fd68559fd1441d6eff2ab7c1e6363582c82febfaa8045df3bd8de",
		Name: "drunk_hawking",
	}

	c3 := &container.Container{
		ID:   "3cdbd1aa394fd68559fd1441d6eff2abfafdcba06e72d2febdba229008b0bf57",
		Name: "3cdbd1aa",
	}

	c4 := &container.Container{
		ID:   "75fb0b800922abdbef2d27e60abcdfaf7fb0698b2a96d22d3354da361a6ff4a5",
		Name: "5a4ff6a163ad4533d22d69a2b8960bf7fafdcba06e72d2febdba229008b0bf57",
	}

	c5 := &container.Container{
		ID:   "d22d69a2b8960bf7fafdcba06e72d2febdba960bf7fafdcba06e72d2f9008b060b",
		Name: "d22d69a2b896",
	}

	store := container.NewMemoryStore()
	store.Add(c1.ID, c1)
	store.Add(c2.ID, c2)
	store.Add(c3.ID, c3)
	store.Add(c4.ID, c4)
	store.Add(c5.ID, c5)

	containersReplica, err := container.NewViewDB()
	if err != nil {
		t.Fatalf("could not create ViewDB: %v", err)
	}

	containersReplica.Save(c1)
	containersReplica.Save(c2)
	containersReplica.Save(c3)
	containersReplica.Save(c4)
	containersReplica.Save(c5)

	daemon := &Daemon{
		containers:        store,
		containersReplica: containersReplica,
	}

	daemon.reserveName(c1.ID, c1.Name)
	daemon.reserveName(c2.ID, c2.Name)
	daemon.reserveName(c3.ID, c3.Name)
	daemon.reserveName(c4.ID, c4.Name)
	daemon.reserveName(c5.ID, c5.Name)

	if ctr, _ := daemon.GetContainer("3cdbd1aa394fd68559fd1441d6eff2ab7c1e6363582c82febfaa8045df3bd8de"); ctr != c2 {
		t.Fatal("Should explicitly match full container IDs")
	}

	if ctr, _ := daemon.GetContainer("75fb0b8009"); ctr != c4 {
		t.Fatal("Should match a partial ID")
	}

	if ctr, _ := daemon.GetContainer("drunk_hawking"); ctr != c2 {
		t.Fatal("Should match a full name")
	}

	// c3.Name is a partial match for both c3.ID and c2.ID
	if c, _ := daemon.GetContainer("3cdbd1aa"); c != c3 {
		t.Fatal("Should match a full name even though it collides with another container's ID")
	}

	if ctr, _ := daemon.GetContainer("d22d69a2b896"); ctr != c5 {
		t.Fatal("Should match a container where the provided prefix is an exact match to the its name, and is also a prefix for its ID")
	}

	if _, err := daemon.GetContainer("3cdbd1"); err == nil {
		t.Fatal("Should return an error when provided a prefix that partially matches multiple container ID's")
	}

	if _, err := daemon.GetContainer("nothing"); err == nil {
		t.Fatal("Should return an error when provided a prefix that is neither a name or a partial match to an ID")
	}
}

func initDaemonWithVolumeStore(tmp string) (*Daemon, error) {
	var err error
	daemon := &Daemon{
		repository: tmp,
		root:       tmp,
	}
	daemon.volumes, err = volumesservice.NewVolumeService(tmp, nil, idtools.Identity{UID: 0, GID: 0}, daemon)
	if err != nil {
		return nil, err
	}
	return daemon, nil
}

func TestValidContainerNames(t *testing.T) {
	invalidNames := []string{"-rm", "&sdfsfd", "safd%sd"}
	validNames := []string{"word-word", "word_word", "1weoid"}

	for _, name := range invalidNames {
		if validContainerNamePattern.MatchString(name) {
			t.Fatalf("%q is not a valid container name and was returned as valid.", name)
		}
	}

	for _, name := range validNames {
		if !validContainerNamePattern.MatchString(name) {
			t.Fatalf("%q is a valid container name and was returned as invalid.", name)
		}
	}
}

func TestContainerInitDNS(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required") // for chown
	}

	tmp, err := os.MkdirTemp("", "docker-container-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	containerID := "d59df5276e7b219d510fe70565e0404bc06350e0d4b43fe961f22f339980170e"
	containerPath := filepath.Join(tmp, containerID)
	if err := os.MkdirAll(containerPath, 0o755); err != nil {
		t.Fatal(err)
	}

	config := `{"State":{"Running":true,"Paused":false,"Restarting":false,"OOMKilled":false,"Dead":false,"Pid":2464,"ExitCode":0,
"Error":"","StartedAt":"2015-05-26T16:48:53.869308965Z","FinishedAt":"0001-01-01T00:00:00Z"},
"ID":"d59df5276e7b219d510fe70565e0404bc06350e0d4b43fe961f22f339980170e","Created":"2015-05-26T16:48:53.7987917Z","Path":"top",
"Args":[],"Config":{"Hostname":"d59df5276e7b","Domainname":"","User":"","Memory":0,"MemorySwap":0,"CpuShares":0,"Cpuset":"",
"AttachStdin":false,"AttachStdout":false,"AttachStderr":false,"PortSpecs":null,"ExposedPorts":null,"Tty":true,"OpenStdin":true,
"StdinOnce":false,"Env":null,"Cmd":["top"],"Image":"ubuntu:latest","Volumes":null,"WorkingDir":"","Entrypoint":null,
"NetworkDisabled":false,"MacAddress":"","OnBuild":null,"Labels":{}},"Image":"07f8e8c5e66084bef8f848877857537ffe1c47edd01a93af27e7161672ad0e95",
"NetworkSettings":{"IPAddress":"172.17.0.1","IPPrefixLen":16,"MacAddress":"02:42:ac:11:00:01","LinkLocalIPv6Address":"fe80::42:acff:fe11:1",
"LinkLocalIPv6PrefixLen":64,"GlobalIPv6Address":"","GlobalIPv6PrefixLen":0,"Gateway":"172.17.42.1","IPv6Gateway":"","Bridge":"docker0","Ports":{}},
"ResolvConfPath":"/var/lib/docker/containers/d59df5276e7b219d510fe70565e0404bc06350e0d4b43fe961f22f339980170e/resolv.conf",
"HostnamePath":"/var/lib/docker/containers/d59df5276e7b219d510fe70565e0404bc06350e0d4b43fe961f22f339980170e/hostname",
"HostsPath":"/var/lib/docker/containers/d59df5276e7b219d510fe70565e0404bc06350e0d4b43fe961f22f339980170e/hosts",
"LogPath":"/var/lib/docker/containers/d59df5276e7b219d510fe70565e0404bc06350e0d4b43fe961f22f339980170e/d59df5276e7b219d510fe70565e0404bc06350e0d4b43fe961f22f339980170e-json.log",
"Name":"/ubuntu","Driver":"aufs","MountLabel":"","ProcessLabel":"","AppArmorProfile":"","RestartCount":0,
"UpdateDns":false,"Volumes":{},"VolumesRW":{},"AppliedVolumesFrom":null}`

	// Container struct only used to retrieve path to config file
	ctr := &container.Container{Root: containerPath}
	configPath, err := ctr.ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	hostConfig := `{"Binds":[],"ContainerIDFile":"","Memory":0,"MemorySwap":0,"CpuShares":0,"CpusetCpus":"",
"Privileged":false,"PortBindings":{},"Links":null,"PublishAllPorts":false,"Dns":null,"DnsOptions":null,"DnsSearch":null,"ExtraHosts":null,"VolumesFrom":null,
"Devices":[],"NetworkMode":"bridge","IpcMode":"","PidMode":"","CapAdd":null,"CapDrop":null,"RestartPolicy":{"Name":"no","MaximumRetryCount":0},
"SecurityOpt":null,"ReadonlyRootfs":false,"Ulimits":null,"LogConfig":{"Type":"","Config":null},"CgroupParent":""}`

	hostConfigPath, err := ctr.HostConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(hostConfigPath, []byte(hostConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	daemon, err := initDaemonWithVolumeStore(tmp)
	if err != nil {
		t.Fatal(err)
	}

	c, err := daemon.load(containerID)
	if err != nil {
		t.Fatal(err)
	}

	if c.HostConfig.DNS == nil {
		t.Fatal("Expected container DNS to not be nil")
	}

	if c.HostConfig.DNSSearch == nil {
		t.Fatal("Expected container DNSSearch to not be nil")
	}

	if c.HostConfig.DNSOptions == nil {
		t.Fatal("Expected container DNSOptions to not be nil")
	}
}

func TestMerge(t *testing.T) {
	configImage := &containertypes.Config{
		ExposedPorts: network.PortSet{
			network.MustParsePort("1111/tcp"): struct{}{},
			network.MustParsePort("2222/tcp"): struct{}{},
		},
		Env: []string{"VAR1=1", "VAR2=2"},
		Volumes: map[string]struct{}{
			"/test1": {},
			"/test2": {},
		},
		Labels: map[string]string{
			"com.docker.foo": "bar",
			"my-label":       "my-value",
		},
	}

	configUser := &containertypes.Config{
		ExposedPorts: network.PortSet{
			network.MustParsePort("2222/tcp"): struct{}{},
			network.MustParsePort("3333/tcp"): struct{}{},
		},
		Env: []string{"VAR2=3", "VAR3=3"},
		Volumes: map[string]struct{}{
			"/test3": {},
		},
	}

	if err := merge(configUser, configImage, mergeOptions{keepReservedLabels: true}); err != nil {
		t.Error(err)
	}

	if len(configUser.ExposedPorts) != 3 {
		t.Fatalf("Expected 3 ExposedPorts, 1111, 2222 and 3333, found %d", len(configUser.ExposedPorts))
	}
	for portSpecs := range configUser.ExposedPorts {
		if portSpecs.Num() != 1111 && portSpecs.Num() != 2222 && portSpecs.Num() != 3333 {
			t.Fatalf("Expected 1111 or 2222 or 3333, found %s", portSpecs)
		}
	}
	if len(configUser.Env) != 3 {
		t.Fatalf("Expected 3 env var, VAR1=1, VAR2=3 and VAR3=3, found %d", len(configUser.Env))
	}
	for _, env := range configUser.Env {
		if env != "VAR1=1" && env != "VAR2=3" && env != "VAR3=3" {
			t.Fatalf("Expected VAR1=1 or VAR2=3 or VAR3=3, found %s", env)
		}
	}

	if len(configUser.Volumes) != 3 {
		t.Fatalf("Expected 3 volumes, /test1, /test2 and /test3, found %d", len(configUser.Volumes))
	}
	for v := range configUser.Volumes {
		if v != "/test1" && v != "/test2" && v != "/test3" {
			t.Fatalf("Expected /test1 or /test2 or /test3, found %s", v)
		}
	}

	assert.DeepEqual(t, configUser.Labels, configImage.Labels)

	configImage2 := &containertypes.Config{
		ExposedPorts: map[network.Port]struct{}{
			network.MustParsePort("0/tcp"): {},
		},
		Labels: map[string]string{
			"com.docker.foo": "bar",
			"my-label":       "my-value",
		},
	}
	configUser2 := &containertypes.Config{
		ExposedPorts: portsUser,
	}

	if err := merge(configUser2, configImage2, mergeOptions{}); err != nil {
		t.Error(err)
	}

	if len(configUser2.ExposedPorts) != 4 {
		t.Fatalf("Expected 4 ExposedPorts, 0000, 1111, 2222 and 3333, found %d", len(configUser.ExposedPorts))
	}
	for portSpecs := range configUser2.ExposedPorts {
		if portSpecs.Num() != 0 && portSpecs.Num() != 1111 && portSpecs.Num() != 2222 && portSpecs.Num() != 3333 {
			t.Fatalf("Expected %d or %d or %d or %d, found %d", 0, 1111, 2222, 3333, portSpecs)
		}
	}
	assert.DeepEqual(t, configUser2.Labels, map[string]string{"my-label": "my-value"})
}

func TestValidateContainerIsolation(t *testing.T) {
	d := Daemon{}

	_, err := d.verifyContainerSettings(&configStore{}, &containertypes.HostConfig{Isolation: containertypes.Isolation("invalid")}, nil, false)
	assert.Check(t, is.Error(err, "invalid isolation 'invalid' on "+runtime.GOOS))
}

func TestInvalidContainerPort0(t *testing.T) {
	d := Daemon{}

	hc := containertypes.HostConfig{
		PortBindings: map[network.Port][]network.PortBinding{
			network.MustParsePort("0/tcp"): {},
		},
	}
	_, err := d.verifyContainerSettings(&configStore{}, &hc, nil, false)
	assert.Error(t, err, `invalid port specification: "0/tcp"`)
}

func TestFindNetworkErrorType(t *testing.T) {
	d := Daemon{}
	_, err := d.FindNetwork("fakeNet")
	var nsn libnetwork.ErrNoSuchNetwork
	ok := errors.As(err, &nsn)
	if !cerrdefs.IsNotFound(err) || !ok {
		t.Error("The FindNetwork method MUST always return an error that implements the NotFound interface and is ErrNoSuchNetwork")
	}
}

// TestDeriveULABaseNetwork checks that for a given hostID, the derived prefix is stable over time.
func TestDeriveULABaseNetwork(t *testing.T) {
	testcases := []struct {
		name      string
		hostID    string
		expPrefix netip.Prefix
	}{
		{
			name:      "Empty hostID",
			expPrefix: netip.MustParsePrefix("fd42:98fc:1c14::/48"),
		},
		{
			name:      "499d4bc0-b0b3-416f-b1ee-cf6486315593",
			hostID:    "499d4bc0-b0b3-416f-b1ee-cf6486315593",
			expPrefix: netip.MustParsePrefix("fd62:fb69:18af::/48"),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			nw := deriveULABaseNetwork(tc.hostID)
			assert.Equal(t, nw.Base, tc.expPrefix)
			assert.Equal(t, nw.Size, 64)
		})
	}
}

// Reading a symlink to a directory must return the directory
func TestResolveSymlinkedDirectoryExistingDirectory(t *testing.T) {
	// TODO Windows: Port this test
	if runtime.GOOS == "windows" {
		t.Skip("Needs porting to Windows")
	}

	// On macOS, tmp itself is symlinked, so resolve this one upfront;
	// see https://github.com/golang/go/issues/56259
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srcPath := filepath.Join(tmpDir, "/testReadSymlinkToExistingDirectory")
	dstPath := filepath.Join(tmpDir, "/dirLinkTest")
	if err = os.Mkdir(srcPath, 0o777); err != nil {
		t.Errorf("failed to create directory: %s", err)
	}

	if err = os.Symlink(srcPath, dstPath); err != nil {
		t.Errorf("failed to create symlink: %s", err)
	}

	var symlinkedPath string
	if symlinkedPath, err = resolveSymlinkedDirectory(dstPath); err != nil {
		t.Fatalf("failed to read symlink to directory: %s", err)
	}

	if symlinkedPath != srcPath {
		t.Fatalf("symlink returned unexpected directory: %s", symlinkedPath)
	}

	if err = os.Remove(srcPath); err != nil {
		t.Errorf("failed to remove temporary directory: %s", err)
	}

	if err = os.Remove(dstPath); err != nil {
		t.Errorf("failed to remove symlink: %s", err)
	}
}

// Reading a non-existing symlink must fail
func TestResolveSymlinkedDirectoryNonExistingSymlink(t *testing.T) {
	tmpDir := t.TempDir()
	symLinkedPath, err := resolveSymlinkedDirectory(path.Join(tmpDir, "/Non/ExistingPath"))
	if err == nil {
		t.Errorf("error expected for non-existing symlink")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Expected an os.ErrNotExist, got: %v", err)
	}
	if symLinkedPath != "" {
		t.Fatalf("expected empty path, but '%s' was returned", symLinkedPath)
	}
}

// Reading a symlink to a file must fail
func TestResolveSymlinkedDirectoryToFile(t *testing.T) {
	// TODO Windows: Port this test
	if runtime.GOOS == "windows" {
		t.Skip("Needs porting to Windows")
	}
	var err error
	var file *os.File

	// #nosec G303
	if file, err = os.Create("/tmp/testSymlinkToFile"); err != nil {
		t.Fatalf("failed to create file: %s", err)
	}

	_ = file.Close()

	if err = os.Symlink("/tmp/testSymlinkToFile", "/tmp/fileLinkTest"); err != nil {
		t.Errorf("failed to create symlink: %s", err)
	}

	symlinkedPath, err := resolveSymlinkedDirectory("/tmp/fileLinkTest")
	if err == nil {
		t.Errorf("resolveSymlinkedDirectory on a symlink to a file should've failed")
	} else if !strings.HasPrefix(err.Error(), "canonical path points to a file") {
		t.Errorf("unexpected error: %v", err)
	}

	if symlinkedPath != "" {
		t.Errorf("path should've been empty: %s", symlinkedPath)
	}

	if err = os.Remove("/tmp/testSymlinkToFile"); err != nil {
		t.Errorf("failed to remove file: %s", err)
	}

	if err = os.Remove("/tmp/fileLinkTest"); err != nil {
		t.Errorf("failed to remove symlink: %s", err)
	}
}
