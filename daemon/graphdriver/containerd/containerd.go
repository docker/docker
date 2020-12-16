// +build linux

package containerd // import "github.com/docker/docker/daemon/graphdriver/containerd"

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/leases"
	mount "github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshots"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/locker"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/stringid"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	driverName = "containerd"

	// namespace used by this graphdriver in containerd
	namespace = "moby"

	// labels used for leveraging "remote" snapshots.
	labelSnapshotRef                   = "containerd.io/snapshot.ref"
	targetRefLabel                     = "containerd.io/snapshot/remote/stargz.reference"
	targetDigestLabel                  = "containerd.io/snapshot/remote/stargz.digest"
	targetImageLayersLabel             = "containerd.io/snapshot/remote/stargz.layers"
	labelSnapshotInflatedContentDiffID = "containerd.io/snapshot/moby.inflated-diff"
	labelSnapshotContentRef            = "containerd.io/snapshot/moby.content-ref"
	labelSnapshotContentNamespace      = "containerd.io/snapshot/moby.content-namespace"

	// labels used for resource management.
	labelGCRoot       = "containerd.io/gc.root"
	labelGCRefContent = "containerd.io/gc.ref.content"
)

// Driver contains information about the home directory and the list of mounts and
// contents that are created using this driver.
type Driver struct {
	home    string
	uidMaps []idtools.IDMap
	gidMaps []idtools.IDMap

	store           content.Store
	snapshotter     snapshots.Snapshotter
	snapshotterName string
	withLease       withLeaseFunc

	locker    *locker.Locker
	naiveDiff graphdriver.DiffDriver

	// TODO: ===== manage these data on disc ======
	contentID  map[string]string // binds layer ids to contents in the content store
	snapshotID map[string]string // binds layer ids to snapshots in the snapshotter
	// ====================================================
}

var (
	logger                 = logrus.WithField("storage-driver", driverName)
	builtinStore           content.Store         // should be used only for tests
	builtinSnapshotter     snapshots.Snapshotter // should be used only for tests
	builtinSnapshotterName string                // should be used only for tests
)

func init() {
	graphdriver.Register(driverName, Init)
}

// Init returns the contaierd-based graphdriver. This driver also
// leverages content store for inflated contents management corresponding to
// snapshots.
func Init(home string, options []string, uidMaps, gidMaps []idtools.IDMap, opt *graphdriver.InitOptions) (graphdriver.Driver, error) {
	var (
		cs     content.Store
		sn     snapshots.Snapshotter
		wl     withLeaseFunc
		snName string
	)
	if name, err := parseName(options); err == nil && opt != nil && opt.ContainerdClient != nil {
		sn = opt.ContainerdClient.SnapshotService(name)
		cs = opt.ContainerdClient.ContentStore()
		wl = withLeaseFuncFromContainerd(opt.ContainerdClient)
		snName = name
	} else if builtinSnapshotter != nil && builtinSnapshotterName != "" && builtinStore != nil {
		// for tests
		sn = builtinSnapshotter
		cs = builtinStore
		wl = func(ctx context.Context) (context.Context, func(context.Context) error, error) {
			return ctx, func(_ context.Context) error { return nil }, nil
		}
		snName = builtinSnapshotterName
	} else {
		return nil, fmt.Errorf("containerd client and snapshotter name must be provided")
	}

	rootUID, rootGID, err := idtools.GetRootUIDGID(uidMaps, gidMaps)
	if err != nil {
		return nil, err
	}
	// Create the driver home dir
	if err := idtools.MkdirAllAndChown(home, 0700, idtools.Identity{UID: rootUID, GID: rootGID}); err != nil {
		return nil, err
	}

	d := &Driver{
		home:            home,
		uidMaps:         uidMaps,
		gidMaps:         gidMaps,
		store:           cs,
		snapshotter:     sn,
		snapshotterName: snName,
		snapshotID:      make(map[string]string),
		contentID:       make(map[string]string),
		locker:          locker.New(),
		withLease:       wl,
	}

	d.naiveDiff = graphdriver.NewNaiveDiffDriver(d, uidMaps, gidMaps)

	return d, nil
}

func (d *Driver) Capabilities() graphdriver.Capabilities {
	// We don't use tar-split and provide diff tar blobs from content store
	// which previously applied by user.
	// TODO: What should be "exact diff" if user didn't invoke "DiffApply"
	//       and didn't provide any diff tarball for this driver?
	return graphdriver.Capabilities{
		ReproducesExactDiffs: true,
	}
}

// String retuns the name of this graphdriver.
func (d *Driver) String() string {
	return driverName
}

// Status returns information about the background snapshotter.
func (d *Driver) Status() [][2]string {
	return [][2]string{
		{"Backing Snapshotter", d.snapshotterName},
	}
}

// GetMetadata returns information about snapshot and content binded to
// the specified layer ID.
func (d *Driver) GetMetadata(id string) (map[string]string, error) {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	var (
		info = make(map[string]string)
		ctx  = namedCtx() // TODO: timeout?
	)

	// Get the information about snapshot
	sID, ok := d.snapshotID[id]
	if !ok {
		return nil, fmt.Errorf("snapshot doesn't registered for given id %q", id)
	}
	sinfo, err := d.snapshotter.Stat(ctx, sID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to stat snapshot %q(key=%q)", id, sID)
	}
	info["SnapshotKind"] = sinfo.Kind.String()
	info["SnapshotName"] = sinfo.Name
	info["SnapshotParent"] = sinfo.Parent
	info["SnapshotCreated"] = sinfo.Created.String()
	info["SnapshotUpdated"] = sinfo.Updated.String()
	info["SnapshotLabels"] = ""
	for k, v := range sinfo.Labels {
		info["SnapshotLabels"] += fmt.Sprintf("%q: %q, ", k, v)
	}

	// Get the information about content
	cID, ok := d.contentID[id]
	if !ok {
		return nil, fmt.Errorf("content doesnt't registered for given id %q", id)
	}
	dgst, err := digest.Parse(cID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse registered cID %q for layer %q", cID, id)
	}
	if st, err := d.store.Status(ctx, id); err == nil {
		// Get the written-in-progress information
		info["ContentRef"] = st.Ref
		info["ContentOffset"] = fmt.Sprintf("%d", st.Offset)
		info["ContentTotal"] = fmt.Sprintf("%d", st.Total)
		info["ContentExpected"] = st.Expected.String()
		info["ContentStartedAt"] = st.StartedAt.String()
		info["ContentUpdatedAt"] = st.UpdatedAt.String()
	} else {
		// Get the committed content information
		cinfo, err := d.store.Info(ctx, dgst)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get info of %q for layer %q", dgst, id)
		}
		info["ContentDigest"] = cinfo.Digest.String()
		info["ContentSize"] = fmt.Sprintf("%d", cinfo.Size)
		info["ContentCreatedAt"] = cinfo.CreatedAt.String()
		info["ContentUpdatedAt"] = cinfo.UpdatedAt.String()
		info["ContentLabels"] = ""
		for k, v := range cinfo.Labels {
			info["ContentLabels"] += fmt.Sprintf("%q: %q, ", k, v)
		}
	}

	return info, nil
}

// Cleanup any state created by the snapshotter when daemon is being shutdown.
func (d *Driver) Cleanup() error {
	if c, ok := d.snapshotter.(snapshots.Cleaner); ok {
		return c.Cleanup(namedCtx())
	}
	return nil
}

// CreateReadWrite creates a new, empty snapshot that is ready to be used as
// the storage for a container.
func (d *Driver) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	return d.create(id, parent, opts)
}

// Create creates a new, empty snapshot with the specified id and parent and
// options passed in opts.
func (d *Driver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	return d.create(id, parent, opts)
}

func (d *Driver) create(id, parent string, opts *graphdriver.CreateOpts) error {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	if _, ok := d.snapshotID[id]; ok {
		return fmt.Errorf("layer (id=%q) already exists", id)
	}

	ctx, done, err := d.withLease(namedCtx()) // TODO: timeout?
	if err != nil {
		return err
	}
	defer done(ctx)

	var psID string
	if parent != "" {
		var ok bool
		psID, ok = d.snapshotID[parent]
		if !ok {
			return fmt.Errorf("snapshot isn't registered for given id %q", parent)
		}
		info, err := d.snapshotter.Stat(ctx, psID)
		if err != nil {
			return errors.Wrapf(err, "failed to stat parent layer %q(key=%q)",
				parent, psID)
		}

		// If the parent snapshot hasn't been committed yet, commit it now so that
		// we can add a new snapshot on the top of it.
		// TODO1: This snapshot still should be able to be modified? Committed snapshot
		//        can't be modified anymore.
		// TODO2: The original active snapshot isn't accessible after this commit. This
		//        means if the user references the mountpoint of active snapshot there
		//        is no guarantee that the mountpoint will be still accssesible.
		//        See also: https://github.com/containerd/containerd/commit/5e8218a63b468ea7ca19fe043c109cda45784570
		if info.Kind != snapshots.KindCommitted {
			labels := info.Labels
			if labels == nil {
				labels = make(map[string]string)
			}

			// We manually manage the lifecycle of this resource.
			labels[labelGCRoot] = time.Now().UTC().Format(time.RFC3339)

			// Refer the content corresponding to this snapshot for preventing the content
			// getting deleted by GC.
			if pcID, ok := d.contentID[parent]; ok {
				labels[labelGCRefContent] = pcID
			}

			var newname string
			for i := 0; i < 3; i++ {
				if newname, err = uniqueKey(); err != nil {
					continue
				}
				if err = d.snapshotter.Commit(ctx, newname, psID, snapshots.WithLabels(labels)); err == nil {
					break
				} else if err != nil && !errdefs.IsAlreadyExists(err) {
					return errors.Wrapf(err, "failed to commit parent layer %q(key=%q)", parent, psID)
				}
				// Key conflicts. try with other key
			}

			// Update key to the committed one
			psID, d.snapshotID[parent] = newname, newname
		}
	}

	var (
		target *graphdriver.TargetOpts
		labels map[string]string
	)
	if opts != nil && opts.TargetInfo != nil {
		// TODO: check all opts are filled
		target = opts.TargetInfo

		// NOTE: The target information of this layer is passed. So we search the snapshot
		// and content in containerd and return "already exist" error if both of them exist.
		// This is usable for "remote layer" functionality which let backing snapshotter
		// to prepare snapshots from underlying remote storages, without pulling and providing
		// image contents from user-side.
		// This leverages containerd's remote snapshotters.
		layers := ""
		for _, l := range target.ImageLayers {
			layers += fmt.Sprintf("%s,", l.String())
		}
		labels = map[string]string{

			// The basic information of targetting snapshot
			labelSnapshotRef:       target.MountChainID.String(),
			targetRefLabel:         target.ImageRef,
			targetDigestLabel:      target.LayerDigest.String(),
			targetImageLayersLabel: layers,

			// The following labels helps snapshotter to prepare contents from backing remote
			// storages and enables us to refer these contents later.
			labelSnapshotInflatedContentDiffID: target.ContentDiffID.String(),
			labelSnapshotContentNamespace:      namespace,
			labelSnapshotContentRef:            id,

			// Refer the content corresponding to this snapshot for preventing the content
			// getting deleted by GC.
			labelGCRefContent: target.ContentDiffID.String(),
		}
	} else {
		labels = make(map[string]string)
	}
	// We manually manage the lifecycle of this resource.
	labels[labelGCRoot] = time.Now().UTC().Format(time.RFC3339)

	// Preapre snapshot
	var sID string
	for i := 0; i < 3; i++ {
		var err error
		if sID, err = uniqueKey(); err != nil {
			continue
		}
		if _, err = d.snapshotter.Prepare(ctx, sID, psID, snapshots.WithLabels(labels)); err == nil {
			// Succeeded to prepare
			d.snapshotID[id] = sID
			return nil
		} else if err != nil && !errdefs.IsAlreadyExists(err) {
			// Failed to prepare
			return errors.Wrapf(err, "failed to prepare snapshot %q for layer %q", sID, id)
		}

		// We are getting already exists error.
		// The possible reasons could be the following so we need to figure out which one here.
		// - Key conflicts.
		// - The snapshot is provided by snapshotter by the ChainID.
		if target == nil || target.MountChainID.String() == "" {
			continue // Key conflicts. try with other key
		}
		chainID := target.MountChainID.String()
		if _, err = d.snapshotter.Stat(ctx, chainID); err != nil {
			continue // Key conflicts. try with other key
		}

		// This layer is provided by snapshotter. Check the content existence.
		sID = chainID
		err = nil
		diffID := target.ContentDiffID
		if diffID.String() == "" {
			d.snapshotter.Remove(ctx, sID)
			return fmt.Errorf("DiffID must be provided not only ChainID for layer %q", id)
		}
		// TODO: Check the Expected digest here but currently containerd doesn't support
		//       getting `Expected` field by `Status`.
		if _, err = d.store.Status(ctx, id); err != nil /* || st.Expected.String() != diffID.String() */ {
			// Corresponding content isn't written-in-progress.
			// Let's check if the content exists as a commtted content.
			if _, err = d.store.Info(ctx, diffID); err != nil {
				d.snapshotter.Remove(ctx, sID)
				return errors.Wrapf(err, "failed to get content (DiffID=%q) for layer %q", diffID, id)
			}
		}

		// The content also exists. Return layer.ErrAlreadyExists.
		d.snapshotID[id] = sID
		d.contentID[id] = diffID.String() // Register the DiffID as a content key

		// Tell the client that this layer exists
		return errors.Wrapf(layer.ErrAlreadyExists, "snapshot and content are alredy exists for layer %q", id)
	}

	return errors.Wrapf(err, "failed to create layer %q", id)
}

// Remove attempts to remove the snapshot and the contents corresponding to the layer.
func (d *Driver) Remove(id string) error {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	sID, ok := d.snapshotID[id]
	if !ok {
		return fmt.Errorf("snapshot doesn't registered for given id %q", id)
	}
	// The referencing content will be garbage collected by containerd
	if err := d.snapshotter.Remove(namedCtx(), sID); err != nil {
		return errors.Wrapf(err, "failed to remove snapshot %q(key=%q)", id, sID)
	}

	delete(d.snapshotID, id)
	delete(d.contentID, id)
	return nil
}

// Get returns the mountpoint for the snapshot referred to by this id.
func (d *Driver) Get(id, mountLabel string) (_ containerfs.ContainerFS, retErr error) {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	sID, ok := d.snapshotID[id]
	if !ok {
		return nil, fmt.Errorf("snapshot isn't registered for given id %q", id)
	}

	ctx, done, err := d.withLease(namedCtx()) // TODO: timeout?
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	dir := d.dir(id)
	rootUID, rootGID, err := idtools.GetRootUIDGID(d.uidMaps, d.gidMaps)
	if err != nil {
		return nil, err
	}
	if err := idtools.MkdirAndChown(dir, 0700, idtools.Identity{UID: rootUID, GID: rootGID}); err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			if rmErr := unix.Rmdir(dir); rmErr != nil && !os.IsNotExist(rmErr) {
				logger.Debugf("Failed to remove %s: %v: %v", id, rmErr, err)
			}
		}
	}()

	info, err := d.snapshotter.Stat(ctx, sID)
	if err != nil {
		retErr = err
		return
	}
	var m []mount.Mount
	if info.Kind == snapshots.KindActive {
		if m, retErr = d.snapshotter.Mounts(ctx, sID); retErr != nil {
			return
		}
	} else {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		labelGCRefSnapshot := fmt.Sprintf("containerd.io/gc.ref.snapshot.%s", d.snapshotterName)

		// readonly view
		for i := 0; i < 3; i++ {
			var vKey string
			vKey, retErr = uniqueKey()
			if retErr != nil {
				continue
			}
			// reference the view for the original snapshot so that the view won't
			// be removed by GC.
			info.Labels[labelGCRefSnapshot] = vKey
			if _, retErr = d.snapshotter.Update(ctx, info, "labels."+labelGCRefSnapshot); retErr != nil {
				retErr = errors.Wrap(err, "failed to configure GC")
				return
			}
			if _, retErr = d.snapshotter.View(ctx, vKey, sID); retErr == nil || !errdefs.IsAlreadyExists(retErr) {
				break
			}
			// Key conflicts. try with other key
		}
		if retErr != nil {
			return
		}
	}
	if err := mount.All(m, dir); err != nil {
		retErr = err
		return
	}
	return containerfs.NewLocalContainerFS(dir), nil
}

// Put unmounts the mount path created for the give id.
func (d *Driver) Put(id string) error {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	dir := d.dir(id)
	if err := mount.Unmount(dir, 0); err != nil {
		return errors.Wrapf(err, "failed to unmount layer %q on %q", id, dir)

	}
	if err := unix.Rmdir(dir); err != nil && !os.IsNotExist(err) {
		logger.Debugf("Failed to remove %s: %v", id, err)
	}
	return nil
}

// Exists returns whether a layer with the specified ID exists on this driver.
func (d *Driver) Exists(id string) bool {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	sID, ok := d.snapshotID[id]
	if !ok {
		return false
	}
	_, err := d.snapshotter.Stat(namedCtx(), sID)
	return err == nil
}

// ApplyDiff applies the new layer into a root
func (d *Driver) ApplyDiff(id string, parent string, diff io.Reader) (int64, error) {
	// TODO: Reduce the restriction; but currently most use-cases in moby seem to be covered?
	if !d.isParent(id, parent) {
		return 0, fmt.Errorf("layer %q isn't registered against %q", id, parent)
	}

	d.locker.Lock(id)
	if _, ok := d.contentID[id]; ok {
		d.locker.Unlock(id)
		return 0, fmt.Errorf("applying diff to %q on %q twice isn't supported", id, parent)
	}
	d.locker.Unlock(id)

	ctx, done, err := d.withLease(namedCtx()) // TODO: timeout?
	if err != nil {
		return 0, err
	}
	defer done(ctx)

	// Open the content writer and provide the diff stream to the
	// content store as well as applying the diff to the targetting snapshot
	cw, err := content.OpenWriter(ctx, d.store, content.WithRef(id))
	if err != nil {
		return 0, err
	}
	defer cw.Close()
	digester := digest.Canonical.Digester()
	dr := io.TeeReader(diff, io.MultiWriter(cw, digester.Hash()))
	applySize, err := d.naiveDiff.ApplyDiff(id, parent, dr)
	if err != nil {
		return 0, err
	}
	io.Copy(ioutil.Discard, dr) // makes sure all contents to be read

	// Configure the prepared snapshot
	d.locker.Lock(id)
	defer d.locker.Unlock(id)
	sID, ok := d.snapshotID[id]
	if !ok {
		if aErr := d.store.Abort(ctx, id); aErr != nil {
			logger.Debugf("Failed to abort %s: %v", id, aErr)
		}
		return 0, fmt.Errorf("snapshot %q isn't registered for %q", id, parent)
	}
	diffID := digester.Digest()
	info, err := d.snapshotter.Stat(ctx, sID)
	if err != nil {
		return 0, err
	}
	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	info.Labels[labelGCRefContent] = diffID.String()
	if _, err := d.snapshotter.Update(ctx, info, "labels."+labelGCRefContent); err != nil {
		if aErr := d.store.Abort(ctx, id); aErr != nil {
			logger.Debugf("Failed to abort %s: %v", id, aErr)
		}
		return 0, errors.Wrap(err, "failed to configure GC")
	}

	// Finally, commit the provided diff contents
	if err := cw.Commit(ctx, 0, diffID); err != nil && !errdefs.IsAlreadyExists(err) {
		if aErr := d.store.Abort(ctx, id); aErr != nil {
			logger.Debugf("Failed to abort %s: %v", id, aErr)
		}
		return 0, err
	}
	d.contentID[id] = diffID.String()

	return applySize, nil
}

// DiffSize calculates the changes between the specified id
// and its parent and returns the size in bytes of the changes
// relative to its base filesystem directory.
func (d *Driver) DiffSize(id, parent string) (size int64, err error) {
	// TODO: Reduce the restriction; but currently most use-cases in moby seem to be covered?
	if !d.isParent(id, parent) {
		return 0, fmt.Errorf("layer diff %q isn't registered against %q", id, parent)
	}

	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	// get the size in the snapshotter
	key, ok := d.snapshotID[id]
	if !ok {
		return 0, fmt.Errorf("layer isn't registered for given id %q", id)
	}
	ctx := namedCtx() // TODO: timeout?
	usage, err := d.snapshotter.Usage(ctx, key)
	if err != nil {
		return 0, err
	}
	size += usage.Size

	// get the size in the content store (if exists the contents)
	if status, err := d.store.Status(ctx, id); err == nil {
		size += status.Total // This is in progress so can be changed
	}
	diffID, ok := d.contentID[id]
	if ok {
		dgst, err := digest.Parse(diffID)
		if err != nil {
			return 0, errors.Wrapf(err, "failed to parse diffID %q of layer %q", diffID, id)
		}
		if info, err := d.store.Info(ctx, dgst); err == nil {
			size += info.Size
		}
	}

	return size, nil
}

// Diff produces an archive of the changes between the specified
// layer and its parent layer which may be "".
func (d *Driver) Diff(id, parent string) (io.ReadCloser, error) {
	// TODO: Reduce the restriction; but currently most use-cases in moby seem to be covered?
	if !d.isParent(id, parent) {
		return nil, fmt.Errorf("layer diff %q isn't registered against %q", id, parent)
	}

	d.locker.Lock(id)
	if _, ok := d.snapshotID[id]; !ok {
		d.locker.Unlock(id)
		return nil, fmt.Errorf("layer %q isn't registered", id)
	}
	cID, ok := d.contentID[id]
	if !ok {
		d.locker.Unlock(id)
		// The content isn't registered by DiffApply. We cannot provide any content.
		// TODO: write it also to the content store.
		return d.naiveDiff.Diff(id, parent)
	}
	d.locker.Unlock(id)

	diffID, err := digest.Parse(cID)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse diffID %q for layer %q", cID, id)
	}
	ctx := namedCtx() // TODO: timeout?
	info, err := d.store.Info(ctx, diffID)
	if err != nil {
		// Wait for writing completion
		doneCh := make(chan content.Info)
		errCh := make(chan error)
		go func() {
			for {
				var err error

				// Check if the content is written in progress
				// TODO: Check the Expected digest here but currently containerd
				//       doesn't support getting `Expected` field by `Status`.
				if _, err = d.store.Status(ctx, id); err == nil /* && st.Expected == diffID */ {
					// writing (diffing) in progress
					time.Sleep(time.Second)
					continue
				}

				// Check if the content exists as a committed content
				if info, err = d.store.Info(ctx, diffID); err == nil {
					doneCh <- info
					return
				}

				// failed to find content
				errCh <- fmt.Errorf("data lost; layer %q: %v", id, err)
				return
			}
		}()
		select {
		case <-time.After(30 * time.Minute):
			return nil, fmt.Errorf("timed out for writing diff content of %q", id)
		case err := <-errCh:
			return nil, errors.Wrapf(err, "failed to get diff of %q", id)
		case info = <-doneCh:
		}
	}
	r, err := d.store.ReaderAt(ctx, ocispec.Descriptor{
		Digest: diffID,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get diff reader of %q", id)
	}
	return ioutil.NopCloser(io.NewSectionReader(r, 0, info.Size)), nil
}

// Changes produces a list of changes between the specified layer and its
// parent layer.
func (d *Driver) Changes(id, parent string) ([]archive.Change, error) {
	return d.naiveDiff.Changes(id, parent)
}

func (d *Driver) isParent(id, parent string) bool {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)

	key, ok := d.snapshotID[id]
	if !ok {
		return false
	}

	pKey := ""
	if parent != "" {
		var ok bool
		pKey, ok = d.snapshotID[parent]
		if !ok {
			return false
		}
	}
	info, err := d.snapshotter.Stat(namedCtx(), key)
	if err != nil {
		return false
	}
	if info.Parent != pKey {
		return false
	}
	return true
}

func (d *Driver) dir(id string) string {
	return path.Join(d.home, id)
}

type withLeaseFunc func(ctx context.Context) (context.Context, func(context.Context) error, error)

func withLeaseFuncFromContainerd(ctd *containerd.Client) func(ctx context.Context) (context.Context, func(context.Context) error, error) {
	lm := ctd.LeasesService()
	return func(ctx context.Context) (context.Context, func(context.Context) error, error) {
		if _, ok := leases.FromContext(ctx); ok {
			return ctx, func(context.Context) error {
				return nil
			}, nil
		}

		l, err := lm.Create(ctx, leases.WithRandomID(), leases.WithExpiration(24*time.Hour))
		if err != nil {
			return nil, nil, err
		}

		ctx = leases.WithLease(ctx, l.ID)
		return ctx, func(ctx context.Context) error {
			return lm.Delete(ctx, l)
		}, nil
	}
}

func namedCtx() context.Context {
	return namespaces.WithNamespace(context.Background(), namespace)
}

func uniqueKey() (string, error) {
	for i := 0; i < 5; i++ {
		key := stringid.GenerateRandomID()
		if _, err := digest.Parse(key); err == nil {
			// Key mustn't conflict with digests.
			// containerd's remote snapshotters uses digests as keys internally
			continue
		}
		return key, nil
	}
	return "", fmt.Errorf("failed to generate unique key that doesn't match digest")
}

func parseName(options []string) (string, error) {
	var names []string
	for _, option := range options {
		key, val, err := parsers.ParseKeyValueOpt(option)
		if err != nil {
			return "", errors.Wrap(err, "failed to parse option")
		}
		key = strings.ToLower(key)
		switch key {
		case "snapshotter.name":
			names = append(names, val)
		default:
			return "", fmt.Errorf("snapshotter: unknown option %s", key)
		}
	}
	if len(names) == 1 && names[0] != "" {
		return names[0], nil
	}
	return "", fmt.Errorf("Exactly one snapshotter name must be specified but got: %v", names)
}
