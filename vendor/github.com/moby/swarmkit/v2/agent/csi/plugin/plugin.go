package plugin

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/moby/swarmkit/v2/api"
	"github.com/moby/swarmkit/v2/internal/csi/capability"
	"github.com/moby/swarmkit/v2/log"
	"github.com/moby/swarmkit/v2/node/plugin"
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

	// scopePath gets the provided path relative to the plugin directory.
	scopePath func(s string) string

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

const (
	// TargetStagePath is the path within the plugin's scope that the volume is
	// to be staged. This does not need to be accessible or propagated outside
	// of the plugin rootfs.
	TargetStagePath string = "/data/staged"
	// TargetPublishPath is the path within the plugin's scope that the volume
	// is to be published. This needs to be the plugin's PropagatedMount.
	TargetPublishPath string = "/data/published"
)

func NewNodePlugin(name string, p plugin.AddrPlugin, secrets SecretGetter) NodePlugin {
	return newNodePlugin(name, p, secrets)
}

// newNodePlugin returns a raw nodePlugin object, not behind an interface. this
// is useful for testing.
func newNodePlugin(name string, p plugin.AddrPlugin, secrets SecretGetter) *nodePlugin {
	return &nodePlugin{
		name:      name,
		socket:    fmt.Sprintf("%s://%s", p.Addr().Network(), p.Addr().String()),
		scopePath: p.ScopedPath,
		secrets:   secrets,
		volumeMap: map[string]*volumePublishStatus{},
	}
}

// connect is a private method that sets up the identity client and node
// client from a grpc client. it exists separately so that testing code can
// substitute in fake clients without a grpc connection
func (np *nodePlugin) connect(ctx context.Context) error {
	// even though this is a unix socket, we must set insecure.NewCredentials or the
	// connection will not be allowed.
	cc, err := grpc.DialContext(ctx, np.socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

func (np *nodePlugin) client(ctx context.Context) (csi.NodeClient, error) {
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

	c, err := np.client(ctx)
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

// GetPublishedPath returns the path at which the provided volume ID is
// published. This path is provided in terms of absolute location on the host,
// not the location in the plugins' scope.
//
// Returns an empty string if the volume does not exist.
func (np *nodePlugin) GetPublishedPath(volumeID string) string {
	np.mu.RLock()
	defer np.mu.RUnlock()
	if volInfo, ok := np.volumeMap[volumeID]; ok {
		if volInfo.isPublished {
			return np.scopePath(volInfo.publishedPath)
		}
	}
	return ""
}

func (np *nodePlugin) NodeGetInfo(ctx context.Context) (*api.NodeCSIInfo, error) {
	c, err := np.client(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := c.NodeGetInfo(ctx, &csi.NodeGetInfoRequest{})
	if err != nil {
		return nil, err
	}

	i := makeNodeInfo(resp)
	i.PluginName = np.name
	return i, nil
}

func (np *nodePlugin) NodeStageVolume(ctx context.Context, req *api.VolumeAssignment) error {
	np.mu.Lock()
	defer np.mu.Unlock()
	if !np.staging {
		return nil
	}

	stagingTarget := stagePath(req)
	err := capability.CheckArguments(req)
	if err != nil {
		return err
	}

	c, err := np.client(ctx)
	if err != nil {
		return err
	}

	_, err = c.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId:          req.VolumeID,
		StagingTargetPath: stagingTarget,
		Secrets:           np.makeSecrets(req),
		VolumeCapability:  capability.MakeCapability(req.AccessMode),
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

	c, err := np.client(ctx)
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
	err := capability.CheckArguments(req)
	if err != nil {
		return err
	}

	np.mu.Lock()
	defer np.mu.Unlock()

	publishTarget := publishPath(req)

	// Some volumes plugins require staging; we track this with a boolean, which
	// also implies a staging path in the path map. If the plugin is marked as
	// requiring staging but does not have a staging path in the map, that is an
	// error.
	var stagingPath string
	if vs, ok := np.volumeMap[req.ID]; ok {
		stagingPath = vs.stagingPath
	} else if np.staging {
		return status.Error(codes.FailedPrecondition, "volume requires staging but was not staged")
	}

	c, err := np.client(ctx)
	if err != nil {
		return err
	}

	_, err = c.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:          req.VolumeID,
		TargetPath:        publishTarget,
		StagingTargetPath: stagingPath,
		VolumeCapability:  capability.MakeCapability(req.AccessMode),
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

	c, err := np.client(ctx)
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
