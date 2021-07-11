package config // import "github.com/docker/docker/integration/config"

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/integration/internal/swarm"
	"github.com/docker/docker/pkg/stdcopy"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestConfigInspect(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	ctx := context.Background()

	testName := t.Name()
	configID := createConfig(ctx, t, c, testName, []byte("TESTINGDATA"), nil)

	insp, body, err := c.ConfigInspectWithRaw(ctx, configID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Name, testName))

	var config swarmtypes.Config
	err = json.Unmarshal(body, &config)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(config, insp))
}

func TestConfigList(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()
	ctx := context.Background()

	// This test case is ported from the original TestConfigsEmptyList
	configs, err := c.ConfigList(ctx, types.ConfigListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(configs), 0))

	testName0 := "test0-" + t.Name()
	testName1 := "test1-" + t.Name()
	testNames := []string{testName0, testName1}
	sort.Strings(testNames)

	// create config test0
	createConfig(ctx, t, c, testName0, []byte("TESTINGDATA0"), map[string]string{"type": "test"})

	config1ID := createConfig(ctx, t, c, testName1, []byte("TESTINGDATA1"), map[string]string{"type": "production"})

	// test by `config ls`
	entries, err := c.ConfigList(ctx, types.ConfigListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(configNamesFromList(entries), testNames))

	testCases := []struct {
		filters  filters.Args
		expected []string
	}{
		// test filter by name `config ls --filter names=xxx`
		{
			filters:  filters.NewArgs(filters.Arg("names", testName0)),
			expected: []string{testName0},
		},
		// test filter by name `config ls --filter name=xxx`
		{
			filters:  filters.NewArgs(filters.Arg("name", testName0)),
			expected: []string{testName0},
		},
		// test filter by id `config ls --filter id=xxx`
		{
			filters:  filters.NewArgs(filters.Arg("id", config1ID)),
			expected: []string{testName1},
		},
		// test filter by label `config ls --filter label=xxx`
		{
			filters:  filters.NewArgs(filters.Arg("label", "type")),
			expected: testNames,
		},
		{
			filters:  filters.NewArgs(filters.Arg("label", "type=test")),
			expected: []string{testName0},
		},
		{
			filters:  filters.NewArgs(filters.Arg("label", "type=production")),
			expected: []string{testName1},
		},
	}
	for _, tc := range testCases {
		entries, err = c.ConfigList(ctx, types.ConfigListOptions{
			Filters: tc.filters,
		})
		assert.NilError(t, err)
		assert.Check(t, is.DeepEqual(configNamesFromList(entries), tc.expected))

	}
}

func createConfig(ctx context.Context, t *testing.T, client client.APIClient, name string, data []byte, labels map[string]string) string {
	config, err := client.ConfigCreate(ctx, swarmtypes.ConfigSpec{
		Annotations: swarmtypes.Annotations{
			Name:   name,
			Labels: labels,
		},
		Data: data,
	})
	assert.NilError(t, err)
	assert.Check(t, config.ID != "")
	return config.ID
}

func TestConfigsCreateAndDelete(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()
	ctx := context.Background()

	testName := "test_config-" + t.Name()
	configID := createConfig(ctx, t, c, testName, []byte("TESTINGDATA"), nil)

	err := c.ConfigRemove(ctx, configID)
	assert.NilError(t, err)

	_, _, err = c.ConfigInspectWithRaw(ctx, configID)
	assert.Check(t, errdefs.IsNotFound(err))
	assert.Check(t, is.ErrorContains(err, configID))

	err = c.ConfigRemove(ctx, "non-existing")
	assert.Check(t, errdefs.IsNotFound(err))
	assert.Check(t, is.ErrorContains(err, "non-existing"))

	testName = "test_secret_with_labels_" + t.Name()
	configID = createConfig(ctx, t, c, testName, []byte("TESTINGDATA"), map[string]string{
		"key1": "value1",
		"key2": "value2",
	})

	insp, _, err := c.ConfigInspectWithRaw(ctx, configID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Name, testName))
	assert.Check(t, is.Equal(len(insp.Spec.Labels), 2))
	assert.Check(t, is.Equal(insp.Spec.Labels["key1"], "value1"))
	assert.Check(t, is.Equal(insp.Spec.Labels["key2"], "value2"))
}

func TestConfigsUpdate(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()
	ctx := context.Background()

	testName := "test_config-" + t.Name()
	configID := createConfig(ctx, t, c, testName, []byte("TESTINGDATA"), nil)

	insp, _, err := c.ConfigInspectWithRaw(ctx, configID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.ID, configID))

	// test UpdateConfig with full ID
	insp.Spec.Labels = map[string]string{"test": "test1"}
	err = c.ConfigUpdate(ctx, configID, insp.Version, insp.Spec)
	assert.NilError(t, err)

	insp, _, err = c.ConfigInspectWithRaw(ctx, configID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Labels["test"], "test1"))

	// test UpdateConfig with full name
	insp.Spec.Labels = map[string]string{"test": "test2"}
	err = c.ConfigUpdate(ctx, testName, insp.Version, insp.Spec)
	assert.NilError(t, err)

	insp, _, err = c.ConfigInspectWithRaw(ctx, configID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Labels["test"], "test2"))

	// test UpdateConfig with prefix ID
	insp.Spec.Labels = map[string]string{"test": "test3"}
	err = c.ConfigUpdate(ctx, configID[:1], insp.Version, insp.Spec)
	assert.NilError(t, err)

	insp, _, err = c.ConfigInspectWithRaw(ctx, configID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(insp.Spec.Labels["test"], "test3"))

	// test UpdateConfig in updating Data which is not supported in daemon
	// this test will produce an error in func UpdateConfig
	insp.Spec.Data = []byte("TESTINGDATA2")
	err = c.ConfigUpdate(ctx, configID, insp.Version, insp.Spec)
	assert.Check(t, errdefs.IsInvalidParameter(err))
	assert.Check(t, is.ErrorContains(err, "only updates to Labels are allowed"))
}

func TestTemplatedConfig(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()
	ctx := context.Background()

	referencedSecretName := "referencedsecret-" + t.Name()
	referencedSecretSpec := swarmtypes.SecretSpec{
		Annotations: swarmtypes.Annotations{
			Name: referencedSecretName,
		},
		Data: []byte("this is a secret"),
	}
	referencedSecret, err := c.SecretCreate(ctx, referencedSecretSpec)
	assert.Check(t, err)

	referencedConfigName := "referencedconfig-" + t.Name()
	referencedConfigSpec := swarmtypes.ConfigSpec{
		Annotations: swarmtypes.Annotations{
			Name: referencedConfigName,
		},
		Data: []byte("this is a config"),
	}
	referencedConfig, err := c.ConfigCreate(ctx, referencedConfigSpec)
	assert.Check(t, err)

	templatedConfigName := "templated_config-" + t.Name()
	configSpec := swarmtypes.ConfigSpec{
		Annotations: swarmtypes.Annotations{
			Name: templatedConfigName,
		},
		Templating: &swarmtypes.Driver{
			Name: "golang",
		},
		Data: []byte("SERVICE_NAME={{.Service.Name}}\n" +
			"{{secret \"referencedsecrettarget\"}}\n" +
			"{{config \"referencedconfigtarget\"}}\n"),
	}

	templatedConfig, err := c.ConfigCreate(ctx, configSpec)
	assert.Check(t, err)

	serviceName := "svc_" + t.Name()
	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithConfig(
			&swarmtypes.ConfigReference{
				File: &swarmtypes.ConfigReferenceFileTarget{
					Name: "templated_config",
					UID:  "0",
					GID:  "0",
					Mode: 0600,
				},
				ConfigID:   templatedConfig.ID,
				ConfigName: templatedConfigName,
			},
		),
		swarm.ServiceWithConfig(
			&swarmtypes.ConfigReference{
				File: &swarmtypes.ConfigReferenceFileTarget{
					Name: "referencedconfigtarget",
					UID:  "0",
					GID:  "0",
					Mode: 0600,
				},
				ConfigID:   referencedConfig.ID,
				ConfigName: referencedConfigName,
			},
		),
		swarm.ServiceWithSecret(
			&swarmtypes.SecretReference{
				File: &swarmtypes.SecretReferenceFileTarget{
					Name: "referencedsecrettarget",
					UID:  "0",
					GID:  "0",
					Mode: 0600,
				},
				SecretID:   referencedSecret.ID,
				SecretName: referencedSecretName,
			},
		),
		swarm.ServiceWithName(serviceName),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(c, serviceID, 1), swarm.ServicePoll, poll.WithTimeout(1*time.Minute))

	tasks := swarm.GetRunningTasks(t, c, serviceID)
	assert.Assert(t, len(tasks) > 0, "no running tasks found for service %s", serviceID)

	attach := swarm.ExecTask(t, d, tasks[0], types.ExecConfig{
		Cmd:          []string{"/bin/cat", "/templated_config"},
		AttachStdout: true,
		AttachStderr: true,
	})

	expect := "SERVICE_NAME=" + serviceName + "\n" +
		"this is a secret\n" +
		"this is a config\n"
	assertAttachedStream(t, attach, expect)

	attach = swarm.ExecTask(t, d, tasks[0], types.ExecConfig{
		Cmd:          []string{"mount"},
		AttachStdout: true,
		AttachStderr: true,
	})
	assertAttachedStream(t, attach, "tmpfs on /templated_config type tmpfs")
}

// Test case for 28884
func TestConfigCreateResolve(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")

	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	ctx := context.Background()

	configName := "test_config_" + t.Name()
	configID := createConfig(ctx, t, c, configName, []byte("foo"), nil)

	fakeName := configID
	fakeID := createConfig(ctx, t, c, fakeName, []byte("fake foo"), nil)

	entries, err := c.ConfigList(ctx, types.ConfigListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.Contains(configNamesFromList(entries), configName))
	assert.Assert(t, is.Contains(configNamesFromList(entries), fakeName))

	err = c.ConfigRemove(ctx, configID)
	assert.NilError(t, err)

	// Fake one will remain
	entries, err = c.ConfigList(ctx, types.ConfigListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(configNamesFromList(entries), []string{fakeName}))

	// Remove based on name prefix of the fake one
	// (which is the same as the ID of foo one) should not work
	// as search is only done based on:
	// - Full ID
	// - Full Name
	// - Partial ID (prefix)
	err = c.ConfigRemove(ctx, configID[:5])
	assert.Assert(t, nil != err)
	entries, err = c.ConfigList(ctx, types.ConfigListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.DeepEqual(configNamesFromList(entries), []string{fakeName}))

	// Remove based on ID prefix of the fake one should succeed
	err = c.ConfigRemove(ctx, fakeID[:5])
	assert.NilError(t, err)
	entries, err = c.ConfigList(ctx, types.ConfigListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(0, len(entries)))
}

func assertAttachedStream(t *testing.T, attach types.HijackedResponse, expect string) {
	buf := bytes.NewBuffer(nil)
	_, err := stdcopy.StdCopy(buf, buf, attach.Reader)
	assert.NilError(t, err)
	assert.Check(t, is.Contains(buf.String(), expect))
}

func configNamesFromList(entries []swarmtypes.Config) []string {
	var values []string
	for _, entry := range entries {
		values = append(values, entry.Spec.Name)
	}
	sort.Strings(values)
	return values
}
