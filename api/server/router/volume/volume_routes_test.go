package volume

import (
	"net/http/httptest"
	"testing"

	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"gotest.tools/v3/assert"

	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/volume/service/opts"
)

func callGetVolume(v *volumeRouter, name string) (*httptest.ResponseRecorder, error) {
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	vars := map[string]string{"name": name}
	req := httptest.NewRequest("GET", fmt.Sprintf("/volumes/%s", name), nil)
	resp := httptest.NewRecorder()

	err := v.getVolumeByName(ctx, resp, req, vars)
	return resp, err
}

func callListVolumes(v *volumeRouter) (*httptest.ResponseRecorder, error) {
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	vars := map[string]string{}
	req := httptest.NewRequest("GET", "/volumes", nil)
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
	assert.Assert(t, errdefs.IsNotFound(err))
}

func TestGetVolumeByNameNotFoundNotManager(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{},
		cluster: &fakeClusterBackend{swarm: true},
	}

	_, err := callGetVolume(v, "notReal")

	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsUnavailable(err))
}

func TestGetVolumeByNameNotFound(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{},
		cluster: &fakeClusterBackend{swarm: true, manager: true},
	}

	_, err := callGetVolume(v, "notReal")

	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsNotFound(err))
}

func TestGetVolumeByNameFoundRegular(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{
			volumes: map[string]*types.Volume{
				"volume1": &types.Volume{
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
			volumes: map[string]*types.Volume{
				"volume1": &types.Volume{
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
			volumes: map[string]*types.Volume{
				"v1": &types.Volume{Name: "v1"},
				"v2": &types.Volume{Name: "v2"},
			},
		},
		cluster: &fakeClusterBackend{
			swarm:   true,
			manager: true,
			volumes: map[string]*types.Volume{
				"v3": &types.Volume{Name: "v3"},
				"v4": &types.Volume{Name: "v4"},
			},
		},
	}

	resp, err := callListVolumes(v)
	assert.NilError(t, err)
	d := json.NewDecoder(resp.Result().Body)
	respVols := volume.VolumeListOKBody{}
	assert.NilError(t, d.Decode(&respVols))

	assert.Assert(t, respVols.Volumes != nil)
	assert.Equal(t, len(respVols.Volumes), 4, "volumes %v", respVols.Volumes)
}

func TestListVolumesNoSwarm(t *testing.T) {
	v := &volumeRouter{
		backend: &fakeVolumeBackend{
			volumes: map[string]*types.Volume{
				"v1": &types.Volume{Name: "v1"},
				"v2": &types.Volume{Name: "v2"},
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
			volumes: map[string]*types.Volume{
				"v1": &types.Volume{Name: "v1"},
				"v2": &types.Volume{Name: "v2"},
			},
		},
		cluster: &fakeClusterBackend{swarm: true},
	}

	resp, err := callListVolumes(v)
	assert.NilError(t, err)

	d := json.NewDecoder(resp.Result().Body)
	respVols := volume.VolumeListOKBody{}
	assert.NilError(t, d.Decode(&respVols))

	assert.Equal(t, len(respVols.Volumes), 2)
	assert.Equal(t, len(respVols.Warnings), 1)
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

	volumeCreate := volume.VolumeCreateBody{
		Name:   "vol1",
		Driver: "foodriver",
	}

	buf := bytes.Buffer{}
	e := json.NewEncoder(&buf)
	e.Encode(volumeCreate)

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("POST", "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err := v.postVolumesCreate(ctx, resp, req, nil)

	assert.NilError(t, err)

	respVolume := types.Volume{}

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

	volumeCreate := volume.VolumeCreateBody{
		ClusterVolumeSpec: &types.ClusterVolumeSpec{},
		Name:              "volCluster",
		Driver:            "someCSI",
	}

	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(volumeCreate)

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("POST", "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err := v.postVolumesCreate(ctx, resp, req, nil)

	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsUnavailable(err))
}

func TestCreateSwarmVolumeNotManager(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{swarm: true}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeCreate := volume.VolumeCreateBody{
		ClusterVolumeSpec: &types.ClusterVolumeSpec{},
		Name:              "volCluster",
		Driver:            "someCSI",
	}

	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(volumeCreate)

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("POST", "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err := v.postVolumesCreate(ctx, resp, req, nil)

	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsUnavailable(err))
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

	volumeCreate := volume.VolumeCreateBody{
		ClusterVolumeSpec: &types.ClusterVolumeSpec{},
		Name:              "volCluster",
		Driver:            "someCSI",
	}

	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(volumeCreate)

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("POST", "/volumes/create", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	err := v.postVolumesCreate(ctx, resp, req, nil)

	assert.NilError(t, err)

	respVolume := types.Volume{}

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
		volumes: map[string]*types.Volume{
			"vol1": &types.Volume{
				Name: "vo1",
				ClusterVolume: &types.ClusterVolume{
					ID: "vol1",
				},
			},
		},
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeUpdate := volume.VolumeUpdateBody{
		Spec: &types.ClusterVolumeSpec{},
	}

	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(volumeUpdate)
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("POST", "/volumes/vol1/update?version=0", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()

	err := v.postVolumesUpdate(ctx, resp, req, map[string]string{"name": "vol1"})
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

	volumeUpdate := volume.VolumeUpdateBody{
		Spec: &types.ClusterVolumeSpec{},
	}

	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(volumeUpdate)
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("POST", "/volumes/vol1/update?version=0", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()

	err := v.postVolumesUpdate(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsUnavailable(err))
}

func TestUpdateVolumeNotFound(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{
		swarm:   true,
		manager: true,
		volumes: map[string]*types.Volume{},
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	volumeUpdate := volume.VolumeUpdateBody{
		Spec: &types.ClusterVolumeSpec{},
	}

	buf := bytes.Buffer{}
	json.NewEncoder(&buf).Encode(volumeUpdate)
	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("POST", "/volumes/vol1/update?version=0", &buf)
	req.Header.Add("Content-Type", "application/json")

	resp := httptest.NewRecorder()

	err := v.postVolumesUpdate(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsNotFound(err))
}

func TestVolumeRemove(t *testing.T) {
	b := &fakeVolumeBackend{
		volumes: map[string]*types.Volume{
			"vol1": &types.Volume{
				Name: "vol1",
			},
		},
	}
	c := &fakeClusterBackend{swarm: true, manager: true}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("DELETE", "/volumes/vol1", nil)
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
		volumes: map[string]*types.Volume{
			"vol1": &types.Volume{
				Name:          "vol1",
				ClusterVolume: &types.ClusterVolume{},
			},
		},
	}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("DELETE", "/volumes/vol1", nil)
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

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("DELETE", "/volumes/vol1", nil)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsNotFound(err), err.Error())
}

func TestVolumeRemoveNotFoundNoManager(t *testing.T) {
	b := &fakeVolumeBackend{}
	c := &fakeClusterBackend{swarm: true}
	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("DELETE", "/volumes/vol1", nil)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.Assert(t, err != nil)
	assert.Assert(t, errdefs.IsUnavailable(err))
}

func TestVolumeRemoveFoundNoSwarm(t *testing.T) {
	b := &fakeVolumeBackend{
		volumes: map[string]*types.Volume{
			"vol1": &types.Volume{
				Name: "vol1",
			},
		},
	}
	c := &fakeClusterBackend{}

	v := &volumeRouter{
		backend: b,
		cluster: c,
	}

	ctx := context.WithValue(context.Background(), httputils.APIVersionKey{}, clusterVolumesVersion)
	req := httptest.NewRequest("DELETE", "/volumes/vol1", nil)
	resp := httptest.NewRecorder()

	err := v.deleteVolumes(ctx, resp, req, map[string]string{"name": "vol1"})
	assert.NilError(t, err)
	assert.Equal(t, len(b.volumes), 0)
}

type fakeVolumeBackend struct {
	volumes map[string]*types.Volume
}

func (b *fakeVolumeBackend) List(_ context.Context, _ filters.Args) ([]*types.Volume, []string, error) {
	volumes := []*types.Volume{}
	for _, v := range b.volumes {
		volumes = append(volumes, v)
	}
	return volumes, nil, nil
}

func (b *fakeVolumeBackend) Get(_ context.Context, name string, _ ...opts.GetOption) (*types.Volume, error) {
	if v, ok := b.volumes[name]; ok {
		return v, nil
	}
	return nil, errdefs.NotFound(fmt.Errorf("volume %s not found", name))
}

func (b *fakeVolumeBackend) Create(_ context.Context, name, driverName string, _ ...opts.CreateOption) (*types.Volume, error) {
	if _, ok := b.volumes[name]; ok {
		// TODO(dperny): return appropriate error type
		return nil, fmt.Errorf("already exists")
	}

	v := &types.Volume{
		Name:   name,
		Driver: driverName,
	}
	if b.volumes == nil {
		b.volumes = map[string]*types.Volume{
			name: v,
		}
	} else {
		b.volumes[name] = v
	}

	return v, nil
}

func (b *fakeVolumeBackend) Remove(_ context.Context, name string, _ ...opts.RemoveOption) error {
	if _, ok := b.volumes[name]; !ok {
		return errdefs.NotFound(fmt.Errorf("volume %s not found", name))
	}

	delete(b.volumes, name)

	return nil
}

func (b *fakeVolumeBackend) Prune(_ context.Context, _ filters.Args) (*types.VolumesPruneReport, error) {
	return nil, nil
}

type fakeClusterBackend struct {
	swarm   bool
	manager bool
	idCount int
	volumes map[string]*types.Volume
}

func (c *fakeClusterBackend) checkSwarm() error {
	if !c.swarm {
		return errdefs.Unavailable(fmt.Errorf("This node is not a swarm manager. Use \"docker swarm init\" or \"docker swarm join\" to connect this node to swarm and try again."))
	} else if !c.manager {
		return errdefs.Unavailable(fmt.Errorf("This node is not a swarm manager. Worker nodes can't be used to view or modify cluster state. Please run this command on a manager node or promote the current node to a manager."))
	}

	return nil
}

func (c *fakeClusterBackend) GetVolume(nameOrId string) (types.Volume, error) {
	if err := c.checkSwarm(); err != nil {
		return types.Volume{}, err
	}

	if v, ok := c.volumes[nameOrId]; ok {
		return *v, nil
	}
	return types.Volume{}, errdefs.NotFound(fmt.Errorf("volume %s not found", nameOrId))
}

func (c *fakeClusterBackend) GetVolumes(options types.VolumeListOptions) ([]*types.Volume, error) {
	if err := c.checkSwarm(); err != nil {
		return nil, err
	}

	volumes := []*types.Volume{}

	for _, v := range c.volumes {
		volumes = append(volumes, v)
	}
	return volumes, nil
}

func (c *fakeClusterBackend) CreateVolume(volumeCreate volume.VolumeCreateBody) (*types.Volume, error) {
	if err := c.checkSwarm(); err != nil {
		return nil, err
	}

	if _, ok := c.volumes[volumeCreate.Name]; ok {
		// TODO(dperny): return appropriate already exists error
		return nil, fmt.Errorf("already exists")
	}

	v := &types.Volume{
		Name:    volumeCreate.Name,
		Driver:  volumeCreate.Driver,
		Labels:  volumeCreate.Labels,
		Options: volumeCreate.DriverOpts,
		Scope:   "global",
	}

	v.ClusterVolume = &types.ClusterVolume{
		ID:   fmt.Sprintf("cluster_%d", c.idCount),
		Spec: *volumeCreate.ClusterVolumeSpec,
	}

	c.idCount = c.idCount + 1
	if c.volumes == nil {
		c.volumes = map[string]*types.Volume{
			v.Name: v,
		}
	} else {
		c.volumes[v.Name] = v
	}

	return v, nil
}

func (c *fakeClusterBackend) RemoveVolume(nameOrId string) error {
	if err := c.checkSwarm(); err != nil {
		return err
	}

	if _, ok := c.volumes[nameOrId]; !ok {
		return errdefs.NotFound(fmt.Errorf("volume %s not found", nameOrId))
	}

	delete(c.volumes, nameOrId)

	return nil
}

func (c *fakeClusterBackend) UpdateVolume(nameOrId string, version uint64, _ volume.VolumeUpdateBody) error {
	if err := c.checkSwarm(); err != nil {
		return err
	}

	if v, ok := c.volumes[nameOrId]; ok {
		if v.ClusterVolume.Meta.Version.Index != version {
			return fmt.Errorf("wrong version")
		}
		v.ClusterVolume.Meta.Version.Index = v.ClusterVolume.Meta.Version.Index + 1
		// for testing, we don't actually need to change anything about the
		// volume object. let's just increment the version so we can see the
		// call happened.
	} else {
		return errdefs.NotFound(fmt.Errorf("volume %q not found", nameOrId))
	}

	return nil
}
