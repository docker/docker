package volume

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/moby/moby/v2/daemon/internal/pruneprogress"

	"gotest.tools/v3/assert"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/daemon/server/volumebackend"
	"github.com/moby/moby/v2/daemon/volume/service/opts"
	"github.com/moby/moby/v2/errdefs"
)

func callGetVolume(v *volumeRouter, name string) (*httptest.ResponseRecorder, error) {
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	vars := map[string]string{"name": name}
	req := httptest.NewRequest(http.MethodGet, "/volumes/"+name, http.NoBody)
	resp := httptest.NewRecorder()

	err := v.getVolumeByName(ctx, resp, req, vars)
	return resp, err
}

func callListVolumes(v *volumeRouter) (*httptest.ResponseRecorder, error) {
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	vars := map[string]string{}
	req := httptest.NewRequest(http.MethodGet, "/volumes", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.getVolumesList(ctx, resp, req, vars)
	return resp, err
}

func TestGetVolumeByNameNotFoundNoSwarm(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{},
		cluster: &fakeClusterBackend{},
	}

	_, err := callGetVolume(v, "notReal")

	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsNotFound(err))
}

func TestGetVolumeByNameNotFoundNotManager(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{},
		cluster: &fakeClusterBackend{swarm: true},
	}

	_, err := callGetVolume(v, "notReal")

	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsNotFound(err))
}

func TestGetVolumeByNameNotFound(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{},
		cluster: &fakeClusterBackend{swarm: true, manager: true},
	}

	_, err := callGetVolume(v, "notReal")

	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsNotFound(err))
}

func TestGetVolumeByNameFoundRegular(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{
			volumes: map[string]volume.Volume{
				"volume1": {
					Name: "volume1",
				},
			},
		},
		cluster: &fakeClusterBackend{swarm: true, manager: true},
	}

	_, err := callGetVolume(v, "volume1")
	assert.NilError(t, err)
}

func TestGetVolumeByNameFoundSwarm(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{},
		cluster: &fakeClusterBackend{
			swarm:   true,
			manager: true,
			volumes: map[string]volume.Volume{
				"volume1": {
					Name: "volume1",
				},
			},
		},
	}

	_, err := callGetVolume(v, "volume1")
	assert.NilError(t, err)
}

func TestListVolumes(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{
			volumes: map[string]volume.Volume{
				"v1": {Name: "v1"},
				"v2": {Name: "v2"},
			},
		},
		cluster: &fakeClusterBackend{
			swarm:   true,
			manager: true,
			volumes: map[string]volume.Volume{
				"v3": {Name: "v3"},
				"v4": {Name: "v4"},
			},
		},
	}

	resp, err := callListVolumes(v)
	assert.NilError(t, err)
	d := json.NewDecoder(resp.Result().Body)
	respVols := volume.ListResponse{}
	assert.NilError(t, d.Decode(&respVols))

	assert.Assert(t, respVols.Volumes != nil)
	assert.Equal(t, len(respVols.Volumes), 4, "volumes %v", respVols.Volumes)
}

func TestListVolumesNoSwarm(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{
			volumes: map[string]volume.Volume{
				"v1": {Name: "v1"},
				"v2": {Name: "v2"},
			},
		},
		cluster: &fakeClusterBackend{},
	}

	_, err := callListVolumes(v)
	assert.NilError(t, err)
}

func TestListVolumesNoManager(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{
			volumes: map[string]volume.Volume{
				"v1": {Name: "v1"},
				"v2": {Name: "v2"},
			},
		},
		cluster: &fakeClusterBackend{swarm: true},
	}

	resp, err := callListVolumes(v)
	assert.NilError(t, err)

	d := json.NewDecoder(resp.Result().Body)
	respVols := volume.ListResponse{}
	assert.NilError(t, d.Decode(&respVols))

	assert.Equal(t, len(respVols.Volumes), 2)
	assert.Equal(t, len(respVols.Warnings), 0)
}

func TestCreateRegularVolume(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{
		swarm:   true,
		manager: true,
	}
	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeCreate := volume.CreateRequest{
		Name:   "vol1",
		Driver: "foodriver",
	}

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(volumeCreate)
	assert.NilError(t, err)

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodPost, "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err = v.postVolumesCreate(ctx, resp, req, nil)
	assert.NilError(t, err)

	respVolume := volume.Volume{}

	assert.NilError(t, json.NewDecoder(resp.Result().Body).Decode(&respVolume))

	assert.Equal(t, respVolume.Name, "vol1")
	assert.Equal(t, respVolume.Driver, "foodriver")

	assert.Equal(t, 1, len(b.volumes))
	assert.Equal(t, 0, len(c.volumes))
}

func TestCreateSwarmVolumeNoSwarm(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeCreate := volume.CreateRequest{
		ClusterVolumeSpec: &volume.ClusterVolumeSpec{},
		Name:              "volCluster",
		Driver:            "someCSI",
	}

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(volumeCreate)
	assert.NilError(t, err)

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodPost, "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err = v.postVolumesCreate(ctx, resp, req, nil)
	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsUnavailable(err))
}

func TestCreateSwarmVolumeNotManager(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{swarm: true}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeCreate := volume.CreateRequest{
		ClusterVolumeSpec: &volume.ClusterVolumeSpec{},
		Name:              "volCluster",
		Driver:            "someCSI",
	}

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(volumeCreate)
	assert.NilError(t, err)

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodPost, "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err = v.postVolumesCreate(ctx, resp, req, nil)
	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsUnavailable(err))
}

func TestCreateVolumeCluster(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{
		swarm:   true,
		manager: true,
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeCreate := volume.CreateRequest{
		ClusterVolumeSpec: &volume.ClusterVolumeSpec{},
		Name:              "volCluster",
		Driver:            "someCSI",
	}

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(volumeCreate)
	assert.NilError(t, err)

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodPost, "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err = v.postVolumesCreate(ctx, resp, req, nil)
	assert.NilError(t, err)

	respVolume := volume.Volume{}

	assert.NilError(t, json.NewDecoder(resp.Result().Body).Decode(&respVolume))

	assert.Equal(t, respVolume.Name, "volCluster")
	assert.Equal(t, respVolume.Driver, "someCSI")

	assert.Equal(t, 0, len(b.volumes))
	assert.Equal(t, 1, len(c.volumes))
}

func TestUpdateVolume(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{
		swarm:   true,
		manager: true,
		volumes: map[string]volume.Volume{
			"vol1": {
				Name: "vo1",
				ClusterVolume: &volume.ClusterVolume{
					ID: "vol1",
				},
			},
		},
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeUpdate := volumebackend.UpdateOptions{
		Spec: &volume.ClusterVolumeSpec{},
	}

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(volumeUpdate)
	assert.NilError(t, err)

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodPost, "/volumes/vol1/update?version=0", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()

	err = v.putVolumesUpdate(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.NilError(t, err)

	assert.Equal(t, c.volumes["vol1"].ClusterVolume.Meta.Version.Index, uint64(1))
}

func TestUpdateVolumeNoSwarm(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeUpdate := volumebackend.UpdateOptions{
		Spec: &volume.ClusterVolumeSpec{},
	}

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(volumeUpdate)
	assert.NilError(t, err)

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodPost, "/volumes/vol1/update?version=0", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()

	err = v.putVolumesUpdate(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsUnavailable(err))
}

func TestUpdateVolumeNotFound(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{
		swarm:   true,
		manager: true,
		volumes: map[string]volume.Volume{},
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeUpdate := volumebackend.UpdateOptions{
		Spec: &volume.ClusterVolumeSpec{},
	}

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(volumeUpdate)
	assert.NilError(t, err)

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodPost, "/volumes/vol1/update?version=0", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()

	err = v.putVolumesUpdate(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsNotFound(err))
}

func TestVolumeRemove(t *testing.T) {
	b := &fakeVolumeBackend{
		volumes: map[string]volume.Volume{
			"vol1": {
				Name: "vol1",
			},
		},
	}
	c := &fakeClusterBackend{swarm: true, manager: true}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodDelete, "/volumes/vol1", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.NilError(t, err)
	assert.Equal(t, len(b.volumes), 0)
}

func TestVolumeRemoveSwarm(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{
		swarm:   true,
		manager: true,
		volumes: map[string]volume.Volume{
			"vol1": {
				Name:          "vol1",
				ClusterVolume: &volume.ClusterVolume{},
			},
		},
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodDelete, "/volumes/vol1", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.NilError(t, err)
	assert.Equal(t, len(c.volumes), 0)
}

func TestVolumeRemoveNotFoundNoSwarm(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{}
	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodDelete, "/volumes/vol1", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsNotFound(err), err.Error())
}

func TestVolumeRemoveNotFoundNoManager(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{swarm: true}
	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodDelete, "/volumes/vol1", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsNotFound(err))
}

func TestVolumeRemoveFoundNoSwarm(t *testing.T) {
	b := &fakeVolumeBackend{
		volumes: map[string]volume.Volume{
			"vol1": {
				Name: "vol1",
			},
		},
	}
	c := &fakeClusterBackend{}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodDelete, "/volumes/vol1", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.NilError(t, err)
	assert.Equal(t, len(b.volumes), 0)
}

func TestVolumeRemoveNoSwarmInUse(t *testing.T) {
	b := &fakeVolumeBackend{
		volumes: map[string]volume.Volume{
			"inuse": {
				Name: "inuse",
			},
		},
	}
	c := &fakeClusterBackend{}
	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodDelete, "/volumes/inuse", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "inuse"})
	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsConflict(err))
}

func TestVolumeRemoveSwarmForce(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{
		swarm:   true,
		manager: true,
		volumes: map[string]volume.Volume{
			"vol1": {
				Name:          "vol1",
				ClusterVolume: &volume.ClusterVolume{},
				Options:       map[string]string{"mustforce": "yes"},
			},
		},
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest(http.MethodDelete, "/volumes/vol1", http.NoBody)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})

	assert.Assert(t, err != nil)
	assert.Assert(t, cerrdefs.IsConflict(err))

	ctx = context.WithValue(t.Context(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req = httptest.NewRequest(http.MethodDelete, "/volumes/vol1?force=1", http.NoBody)
	resp = httptest.NewRecorder()

	err = v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})

	assert.NilError(t, err)
	assert.Equal(t, len(b.volumes), 0)
	assert.Equal(t, len(c.volumes), 0)
}

type fakeVolumeBackend struct {
	volumes map[string]volume.Volume
}

func (b *fakeVolumeBackend) List(_ context.Context, _ filters.Args) ([]volume.Volume, []string, error) {
	var volumes []volume.Volume
	for _, v := range b.volumes {
		volumes = append(volumes, v)
	}
	return volumes, nil, nil
}

func (b *fakeVolumeBackend) Get(_ context.Context, name string, _ ...opts.GetOption) (*volume.Volume, error) {
	if v, ok := b.volumes[name]; ok {
		return &v, nil
	}
	return nil, errdefs.NotFound(fmt.Errorf("volume %s not found", name))
}

func (b *fakeVolumeBackend) Create(_ context.Context, name, driverName string, _ ...opts.CreateOption) (*volume.Volume, error) {
	if _, ok := b.volumes[name]; ok {
		// TODO(dperny): return appropriate error type
		return nil, errors.New("already exists")
	}

	v := volume.Volume{
		Name:   name,
		Driver: driverName,
	}
	if b.volumes == nil {
		b.volumes = map[string]volume.Volume{
			name: v,
		}
	} else {
		b.volumes[name] = v
	}

	return &v, nil
}

func (b *fakeVolumeBackend) Remove(_ context.Context, name string, o ...opts.RemoveOption) error {
	removeOpts := &opts.RemoveConfig{}
	for _, opt := range o {
		opt(removeOpts)
	}

	if v, ok := b.volumes[name]; !ok {
		if !removeOpts.PurgeOnError {
			return errdefs.NotFound(fmt.Errorf("volume %s not found", name))
		}
	} else if v.Name == "inuse" {
		return errdefs.Conflict(errors.New("volume in use"))
	}

	delete(b.volumes, name)

	return nil
}

func (b *fakeVolumeBackend) Prune(_ context.Context, _ pruneprogress.Options) (*volume.PruneReport, error) {
	return nil, nil
}

type fakeClusterBackend struct {
	swarm   bool
	manager bool
	idCount int
	volumes map[string]volume.Volume
}

func (c *fakeClusterBackend) checkSwarm() error {
	if !c.swarm {
		return errdefs.Unavailable(errors.New("this node is not a swarm manager. Use \"docker swarm init\" or \"docker swarm join\" to connect this node to swarm and try again"))
	} else if !c.manager {
		return errdefs.Unavailable(errors.New("this node is not a swarm manager. Worker nodes can't be used to view or modify cluster state. Please run this command on a manager node or promote the current node to a manager"))
	}

	return nil
}

func (c *fakeClusterBackend) IsManager() bool {
	return c.swarm && c.manager
}

func (c *fakeClusterBackend) GetVolume(nameOrID string) (volume.Volume, error) {
	if err := c.checkSwarm(); err != nil {
		return volume.Volume{}, err
	}

	if v, ok := c.volumes[nameOrID]; ok {
		return v, nil
	}
	return volume.Volume{}, errdefs.NotFound(fmt.Errorf("volume %s not found", nameOrID))
}

func (c *fakeClusterBackend) GetVolumes(_ volumebackend.ListOptions) ([]volume.Volume, error) {
	if err := c.checkSwarm(); err != nil {
		return nil, err
	}

	var volumes []volume.Volume
	for _, v := range c.volumes {
		volumes = append(volumes, v)
	}
	return volumes, nil
}

func (c *fakeClusterBackend) CreateVolume(volumeCreate volume.CreateRequest) (*volume.Volume, error) {
	if err := c.checkSwarm(); err != nil {
		return nil, err
	}

	if _, ok := c.volumes[volumeCreate.Name]; ok {
		// TODO(dperny): return appropriate already exists error
		return nil, errors.New("already exists")
	}

	v := volume.Volume{
		Name:    volumeCreate.Name,
		Driver:  volumeCreate.Driver,
		Labels:  volumeCreate.Labels,
		Options: volumeCreate.DriverOpts,
		Scope:   "global",
		ClusterVolume: &volume.ClusterVolume{
			ID:   fmt.Sprintf("cluster_%d", c.idCount),
			Spec: *volumeCreate.ClusterVolumeSpec,
		},
	}

	c.idCount = c.idCount + 1
	if c.volumes == nil {
		c.volumes = map[string]volume.Volume{
			v.Name: v,
		}
	} else {
		c.volumes[v.Name] = v
	}

	return &v, nil
}

func (c *fakeClusterBackend) RemoveVolume(nameOrID string, force bool) error {
	if err := c.checkSwarm(); err != nil {
		return err
	}

	v, ok := c.volumes[nameOrID]
	if !ok {
		return errdefs.NotFound(fmt.Errorf("volume %s not found", nameOrID))
	}

	if _, mustforce := v.Options["mustforce"]; mustforce && !force {
		return errdefs.Conflict(fmt.Errorf("volume %s must be force removed", nameOrID))
	}

	delete(c.volumes, nameOrID)

	return nil
}

func (c *fakeClusterBackend) UpdateVolume(nameOrID string, version uint64, _ volumebackend.UpdateOptions) error {
	if err := c.checkSwarm(); err != nil {
		return err
	}

	if v, ok := c.volumes[nameOrID]; ok {
		if v.ClusterVolume.Meta.Version.Index != version {
			return errors.New("wrong version")
		}
		v.ClusterVolume.Meta.Version.Index = v.ClusterVolume.Meta.Version.Index + 1
		// for testing, we don't actually need to change anything about the
		// volume object. let's just increment the version so we can see the
		// call happened.
	} else {
		return errdefs.NotFound(fmt.Errorf("volume %q not found", nameOrID))
	}

	return nil
}
