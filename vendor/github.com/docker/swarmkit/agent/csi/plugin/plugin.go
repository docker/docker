package plugin

import (
	"context"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
)

// SecretGetter is a reimplementation of the exec.SecretGetter interface in the
// scope of the plugin package. This avoids the needing to import exec into the
// plugin package.
type SecretGetter interface {
	Get(secretID string) (*api.Secret, error)
}

type NodePlugin interface {
	GetPublishedPath(volumeID string) string
	NodeGetInfo(ctx context.Context) (*api.NodeCSIInfo, error)
	NodeStageVolume(ctx context.Context, req *api.VolumeAssignment) error
	NodeUnstageVolume(ctx context.Context, req *api.VolumeAssignment) error
	NodePublishVolume(ctx context.Context, req *api.VolumeAssignment) error
	NodeUnpublishVolume(ctx context.Context, req *api.VolumeAssignment) error
}

type volumePublishStatus struct {
	// stagingPath is staging path of volume
	stagingPath string

	// isPublished keeps track if the volume is published.
	isPublished bool

	// publishedPath is published path of volume
	publishedPath string
}

type nodePlugin struct {
	// name is the name of the plugin, which is used in the Driver.Name field.
	name string

	// socket is the path of the unix socket to connect to this plugin at
	socket string

	// secrets is the SecretGetter to get volume secret data
	secrets SecretGetter

	// volumeMap is the map from volume ID to Volume. Will place a volume once it is staged,
	// remove it from the map for unstage.
	// TODO: Make this map persistent if the swarm node goes down
	volumeMap map[string]*volumePublishStatus

	// mu for volumeMap
	mu sync.RWMutex

	// staging indicates that the plugin has staging capabilities.
	staging bool

	// cc is the gRPC client connection
	cc *grpc.ClientConn

	// idClient is the CSI Identity Service client
	idClient csi.IdentityClient

	// nodeClient is the CSI Node Service client
	nodeClient csi.NodeClient
}

const TargetStagePath string = "/var/lib/docker/stage"
const TargetPublishPath string = "/var/lib/docker/publish"

func NewNodePlugin(name, socket string, secrets SecretGetter) NodePlugin {
	return newNodePlugin(name, socket, secrets)
}

// newNodePlugin returns a raw nodePlugin object, not behind an interface. this
// is useful for testing.
func newNodePlugin(name, socket string, secrets SecretGetter) *nodePlugin {
	return &nodePlugin{
		name:      name,
		socket:    socket,
		secrets:   secrets,
		volumeMap: map[string]*volumePublishStatus{},
	}
}

// connect is a private method that sets up the identity client and node
// client from a grpc client. it exists separately so that testing code can
// substitute in fake clients without a grpc connection
func (np *nodePlugin) connect(ctx context.Context) error {
	// even though this is a unix socket, we must set WithInsecure or the
	// connection will not be allowed.
	cc, err := grpc.DialContext(ctx, np.socket, grpc.WithInsecure())
	if err != nil {
		return err
	}

	np.cc = cc
	// first, probe the plugin, to ensure that it exists and is ready to go
	idc := csi.NewIdentityClient(cc)
	np.idClient = idc

	np.nodeClient = csi.NewNodeClient(cc)

	return np.init(ctx)
}

func (np *nodePlugin) Client(ctx context.Context) (csi.NodeClient, error) {
	if np.nodeClient == nil {
		if err := np.connect(ctx); err != nil {
			return nil, err
		}
	}
	return np.nodeClient, nil
}

func (np *nodePlugin) init(ctx context.Context) error {
	probe, err := np.idClient.Probe(ctx, &csi.ProbeRequest{})
	if err != nil {
		return err
	}
	if probe.Ready != nil && !probe.Ready.Value {
		return status.Error(codes.FailedPrecondition, "Plugin is not Ready")
	}

	c, err := np.Client(ctx)
	if err != nil {
		return err
	}

	resp, err := c.NodeGetCapabilities(ctx, &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		// TODO(ameyag): handle
		return err
	}
	if resp == nil {
		return nil
	}
	log.G(ctx).Debugf("plugin advertises %d capabilities", len(resp.Capabilities))
	for _, c := range resp.Capabilities {
		if rpc := c.GetRpc(); rpc != nil {
			log.G(ctx).Debugf("plugin has capability %s", rpc)
			switch rpc.Type {
			case csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME:
				np.staging = true
			}
		}
	}

	return nil
}

// GetPublishedPath returns the path at which the provided volume ID is published.
// Returns an empty string if the volume does not exist.
func (np *nodePlugin) GetPublishedPath(volumeID string) string {
	np.mu.RLock()
	defer np.mu.RUnlock()
	if volInfo, ok := np.volumeMap[volumeID]; ok {
		if volInfo.isPublished {
			return volInfo.publishedPath
		}
	}
	return ""
}

func (np *nodePlugin) NodeGetInfo(ctx context.Context) (*api.NodeCSIInfo, error) {
	c, err := np.Client(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
	if err != nil {
		return nil, err
	}

	return makeNodeInfo(resp), nil
}

func (np *nodePlugin) NodeStageVolume(ctx context.Context, req *api.VolumeAssignment) error {
	np.mu.Lock()
	defer np.mu.Unlock()
	if !np.staging {
		return nil
	}

	stagingTarget := stagePath(req)

	// Check arguments
	if len(req.VolumeID) == 0 {
		return status.Error(codes.InvalidArgument, "VolumeID missing in request")
	}

	c, err := np.Client(ctx)
	if err != nil {
		return err
	}

	_, err = c.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          req.VolumeID,
		StagingTargetPath: stagingTarget,
		Secrets:           np.makeSecrets(req),
		VolumeCapability:  makeCapability(req.AccessMode),
		VolumeContext:     req.VolumeContext,
		PublishContext:    req.PublishContext,
	})

	if err != nil {
		return err
	}

	v := &volumePublishStatus{
		stagingPath: stagingTarget,
	}

	np.volumeMap[req.ID] = v

	log.G(ctx).Infof("volume staged to path %s", stagingTarget)
	return nil
}

func (np *nodePlugin) NodeUnstageVolume(ctx context.Context, req *api.VolumeAssignment) error {
	np.mu.Lock()
	defer np.mu.Unlock()
	if !np.staging {
		return nil
	}

	stagingTarget := stagePath(req)

	// Check arguments
	if len(req.VolumeID) == 0 {
		return status.Error(codes.FailedPrecondition, "VolumeID missing in request")
	}

	c, err := np.Client(ctx)
	if err != nil {
		return err
	}

	// we must unpublish before we unstage. verify here that the volume is not
	// published.
	if v, ok := np.volumeMap[req.ID]; ok {
		if v.isPublished {
			return status.Errorf(codes.FailedPrecondition, "Volume %s is not unpublished", req.ID)
		}
		return nil
	}

	_, err = c.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId:          req.VolumeID,
		StagingTargetPath: stagingTarget,
	})
	if err != nil {
		return err
	}

	// if the volume doesn't exist in the volumeMap, deleting has no effect.
	delete(np.volumeMap, req.ID)
	log.G(ctx).Info("volume unstaged")

	return nil
}

func (np *nodePlugin) NodePublishVolume(ctx context.Context, req *api.VolumeAssignment) error {
	// Check arguments
	if len(req.VolumeID) == 0 {
		return status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	np.mu.Lock()
	defer np.mu.Unlock()

	publishTarget := publishPath(req)

	// some volumes do not require staging. we can check this by checkign the
	// staging variable, or we can just see if there is a staging path in the
	// map.
	var stagingPath string
	if vs, ok := np.volumeMap[req.ID]; ok {
		stagingPath = vs.stagingPath
	} else {
		return status.Error(codes.FailedPrecondition, "volume not staged")
	}

	c, err := np.Client(ctx)
	if err != nil {
		return err
	}

	_, err = c.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          req.VolumeID,
		TargetPath:        publishTarget,
		StagingTargetPath: stagingPath,
		VolumeCapability:  makeCapability(req.AccessMode),
		Secrets:           np.makeSecrets(req),
		VolumeContext:     req.VolumeContext,
		PublishContext:    req.PublishContext,
	})
	if err != nil {
		return err
	}

	status, ok := np.volumeMap[req.ID]
	if !ok {
		status = &volumePublishStatus{}
		np.volumeMap[req.ID] = status
	}

	status.isPublished = true
	status.publishedPath = publishTarget

	log.G(ctx).Infof("volume published to path %s", publishTarget)

	return nil
}

func (np *nodePlugin) NodeUnpublishVolume(ctx context.Context, req *api.VolumeAssignment) error {
	// Check arguments
	if len(req.VolumeID) == 0 {
		return status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	np.mu.Lock()
	defer np.mu.Unlock()
	publishTarget := publishPath(req)

	c, err := np.Client(ctx)
	if err != nil {
		return err
	}

	_, err = c.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   req.VolumeID,
		TargetPath: publishTarget,
	})

	if err != nil {
		return err
	}

	if v, ok := np.volumeMap[req.ID]; ok {
		v.publishedPath = ""
		v.isPublished = false
		return nil
	}

	log.G(ctx).Info("volume unpublished")
	return nil
}

func (np *nodePlugin) makeSecrets(v *api.VolumeAssignment) map[string]string {
	// this should never happen, but program defensively.
	if v == nil {
		return nil
	}

	secrets := make(map[string]string, len(v.Secrets))
	for _, secret := range v.Secrets {
		// TODO(dperny): handle error from Get
		value, _ := np.secrets.Get(secret.Secret)
		if value != nil {
			secrets[secret.Key] = string(value.Spec.Data)
		}
	}

	return secrets
}

// makeNodeInfo converts a csi.NodeGetInfoResponse object into a swarmkit NodeCSIInfo
// object.
func makeNodeInfo(csiNodeInfo *csi.NodeGetInfoResponse) *api.NodeCSIInfo {
	return &api.NodeCSIInfo{
		NodeID:            csiNodeInfo.NodeId,
		MaxVolumesPerNode: csiNodeInfo.MaxVolumesPerNode,
	}
}

func makeCapability(am *api.VolumeAccessMode) *csi.VolumeCapability {
	var mode csi.VolumeCapability_AccessMode_Mode
	switch am.Scope {
	case api.VolumeScopeSingleNode:
		switch am.Sharing {
		case api.VolumeSharingNone, api.VolumeSharingOneWriter, api.VolumeSharingAll:
			mode = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
		case api.VolumeSharingReadOnly:
			mode = csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
		}
	case api.VolumeScopeMultiNode:
		switch am.Sharing {
		case api.VolumeSharingReadOnly:
			mode = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
		case api.VolumeSharingOneWriter:
			mode = csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER
		case api.VolumeSharingAll:
			mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		}
	}

	capability := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: mode,
		},
	}

	if block := am.GetBlock(); block != nil {
		capability.AccessType = &csi.VolumeCapability_Block{
			// Block type is empty.
			Block: &csi.VolumeCapability_BlockVolume{},
		}
	}

	if mount := am.GetMount(); mount != nil {
		capability.AccessType = &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{
				FsType:     mount.FsType,
				MountFlags: mount.MountFlags,
			},
		}
	}
	return capability
}

// stagePath returns the staging path for a given volume assignment
func stagePath(v *api.VolumeAssignment) string {
	// this really just exists so we use the same trick to determine staging
	// path across multiple methods and can't forget to change it in one place
	// but not another
	return filepath.Join(TargetStagePath, v.ID)
}

// publishPath returns the publishing path for a given volume assignment
func publishPath(v *api.VolumeAssignment) string {
	// ditto as stagePath
	return filepath.Join(TargetPublishPath, v.ID)
}
