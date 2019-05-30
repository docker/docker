/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package local

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/filters"
	"github.com/containerd/containerd/log"
	"github.com/sirupsen/logrus"

	"github.com/containerd/continuity"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		buffer := make([]byte, 1<<20)
		return &buffer
	},
}

// LabelStore is used to store mutable labels for digests
type LabelStore interface {
	// Get returns all the labels for the given digest
	Get(digest.Digest) (map[string]string, error)

	// Set sets all the labels for a given digest
	Set(digest.Digest, map[string]string) error

	// Update replaces the given labels for a digest,
	// a key with an empty value removes a label.
	Update(digest.Digest, map[string]string) (map[string]string, error)
}

// Store is digest-keyed store for content. All data written into the store is
// stored under a verifiable digest.
//
// Store can generally support multi-reader, single-writer ingest of data,
// including resumable ingest.
type store struct {
	root string
	ls   LabelStore
}

// NewStore returns a local content store
func NewStore(root string) (content.Store, error) {
	return NewLabeledStore(root, nil)
}

// NewLabeledStore returns a new content store using the provided label store
//
// Note: content stores which are used underneath a metadata store may not
// require labels and should use `NewStore`. `NewLabeledStore` is primarily
// useful for tests or standalone implementations.
func NewLabeledStore(root string, ls LabelStore) (content.Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "ingest"), 0777); err != nil {
		return nil, err
	}

	return &store{
		root: root,
		ls:   ls,
	}, nil
}

func (s *store) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	p := s.blobPath(dgst)
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Wrapf(errdefs.ErrNotFound, "content %v", dgst)
		}

		return content.Info{}, err
	}
	var labels map[string]string
	if s.ls != nil {
		labels, err = s.ls.Get(dgst)
		if err != nil {
			return content.Info{}, err
		}
	}
	return s.info(dgst, fi, labels), nil
}

func (s *store) info(dgst digest.Digest, fi os.FileInfo, labels map[string]string) content.Info {
	return content.Info{
		Digest:    dgst,
		Size:      fi.Size(),
		CreatedAt: fi.ModTime(),
		UpdatedAt: getATime(fi),
		Labels:    labels,
	}
}

// ReaderAt returns an io.ReaderAt for the blob.
func (s *store) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	p := s.blobPath(desc.Digest)
	fi, err := os.Stat(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}

		return nil, errors.Wrapf(errdefs.ErrNotFound, "blob %s expected at %s", desc.Digest, p)
	}

	fp, err := os.Open(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}

		return nil, errors.Wrapf(errdefs.ErrNotFound, "blob %s expected at %s", desc.Digest, p)
	}

	return sizeReaderAt{size: fi.Size(), fp: fp}, nil
}

// Delete removes a blob by its digest.
//
// While this is safe to do concurrently, safe exist-removal logic must hold
// some global lock on the store.
func (s *store) Delete(ctx context.Context, dgst digest.Digest) error {
	if err := os.RemoveAll(s.blobPath(dgst)); err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		return errors.Wrapf(errdefs.ErrNotFound, "content %v", dgst)
	}

	return nil
}

func (s *store) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	if s.ls == nil {
		return content.Info{}, errors.Wrapf(errdefs.ErrFailedPrecondition, "update not supported on immutable content store")
	}

	p := s.blobPath(info.Digest)
	fi, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Wrapf(errdefs.ErrNotFound, "content %v", info.Digest)
		}

		return content.Info{}, err
	}

	var (
		all    bool
		labels map[string]string
	)
	if len(fieldpaths) > 0 {
		for _, path := range fieldpaths {
			if strings.HasPrefix(path, "labels.") {
				if labels == nil {
					labels = map[string]string{}
				}

				key := strings.TrimPrefix(path, "labels.")
				labels[key] = info.Labels[key]
				continue
			}

			switch path {
			case "labels":
				all = true
				labels = info.Labels
			default:
				return content.Info{}, errors.Wrapf(errdefs.ErrInvalidArgument, "cannot update %q field on content info %q", path, info.Digest)
			}
		}
	} else {
		all = true
		labels = info.Labels
	}

	if all {
		err = s.ls.Set(info.Digest, labels)
	} else {
		labels, err = s.ls.Update(info.Digest, labels)
	}
	if err != nil {
		return content.Info{}, err
	}

	info = s.info(info.Digest, fi, labels)
	info.UpdatedAt = time.Now()

	if err := os.Chtimes(p, info.UpdatedAt, info.CreatedAt); err != nil {
		log.G(ctx).WithError(err).Warnf("could not change access time for %s", info.Digest)
	}

	return info, nil
}

func (s *store) Walk(ctx context.Context, fn content.WalkFunc, filters ...string) error {
	// TODO: Support filters
	root := filepath.Join(s.root, "blobs")
	var alg digest.Algorithm
	return filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && !alg.Available() {
			return nil
		}

		// TODO(stevvooe): There are few more cases with subdirs that should be
		// handled in case the layout gets corrupted. This isn't strict enough
		// and may spew bad data.

		if path == root {
			return nil
		}
		if filepath.Dir(path) == root {
			alg = digest.Algorithm(filepath.Base(path))

			if !alg.Available() {
				alg = ""
				return filepath.SkipDir
			}

			// descending into a hash directory
			return nil
		}

		dgst := digest.NewDigestFromHex(alg.String(), filepath.Base(path))
		if err := dgst.Validate(); err != nil {
			// log error but don't report
			log.L.WithError(err).WithField("path", path).Error("invalid digest for blob path")
			// if we see this, it could mean some sort of corruption of the
			// store or extra paths not expected previously.
		}

		var labels map[string]string
		if s.ls != nil {
			labels, err = s.ls.Get(dgst)
			if err != nil {
				return err
			}
		}
		return fn(s.info(dgst, fi, labels))
	})
}

func (s *store) Status(ctx context.Context, ref string) (content.Status, error) {
	return s.status(s.ingestRoot(ref))
}

func (s *store) ListStatuses(ctx context.Context, fs ...string) ([]content.Status, error) {
	fp, err := os.Open(filepath.Join(s.root, "ingest"))
	if err != nil {
		return nil, err
	}

	defer fp.Close()

	fis, err := fp.Readdir(-1)
	if err != nil {
		return nil, err
	}

	filter, err := filters.ParseAll(fs...)
	if err != nil {
		return nil, err
	}

	var active []content.Status
	for _, fi := range fis {
		p := filepath.Join(s.root, "ingest", fi.Name())
		stat, err := s.status(p)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, err
			}

			// TODO(stevvooe): This is a common error if uploads are being
			// completed while making this listing. Need to consider taking a
			// lock on the whole store to coordinate this aspect.
			//
			// Another option is to cleanup downloads asynchronously and
			// coordinate this method with the cleanup process.
			//
			// For now, we just skip them, as they really don't exist.
			continue
		}

		if filter.Match(adaptStatus(stat)) {
			active = append(active, stat)
		}
	}

	return active, nil
}

// WalkStatusRefs is used to walk all status references
// Failed status reads will be logged and ignored, if
// this function is called while references are being altered,
// these error messages may be produced.
func (s *store) WalkStatusRefs(ctx context.Context, fn func(string) error) error {
	fp, err := os.Open(filepath.Join(s.root, "ingest"))
	if err != nil {
		return err
	}

	defer fp.Close()

	fis, err := fp.Readdir(-1)
	if err != nil {
		return err
	}

	for _, fi := range fis {
		rf := filepath.Join(s.root, "ingest", fi.Name(), "ref")

		ref, err := readFileString(rf)
		if err != nil {
			log.G(ctx).WithError(err).WithField("path", rf).Error("failed to read ingest ref")
			continue
		}

		if err := fn(ref); err != nil {
			return err
		}
	}

	return nil
}

// status works like stat above except uses the path to the ingest.
func (s *store) status(ingestPath string) (content.Status, error) {
	dp := filepath.Join(ingestPath, "data")
	fi, err := os.Stat(dp)
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Wrap(errdefs.ErrNotFound, err.Error())
		}
		return content.Status{}, err
	}

	ref, err := readFileString(filepath.Join(ingestPath, "ref"))
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Wrap(errdefs.ErrNotFound, err.Error())
		}
		return content.Status{}, err
	}

	startedAt, err := readFileTimestamp(filepath.Join(ingestPath, "startedat"))
	if err != nil {
		return content.Status{}, errors.Wrapf(err, "could not read startedat")
	}

	updatedAt, err := readFileTimestamp(filepath.Join(ingestPath, "updatedat"))
	if err != nil {
		return content.Status{}, errors.Wrapf(err, "could not read updatedat")
	}

	// because we don't write updatedat on every write, the mod time may
	// actually be more up to date.
	if fi.ModTime().After(updatedAt) {
		updatedAt = fi.ModTime()
	}

	return content.Status{
		Ref:       ref,
		Offset:    fi.Size(),
		Total:     s.total(ingestPath),
		UpdatedAt: updatedAt,
		StartedAt: startedAt,
	}, nil
}

func adaptStatus(status content.Status) filters.Adaptor {
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}
		switch fieldpath[0] {
		case "ref":
			return status.Ref, true
		}

		return "", false
	})
}

// total attempts to resolve the total expected size for the write.
func (s *store) total(ingestPath string) int64 {
	totalS, err := readFileString(filepath.Join(ingestPath, "total"))
	if err != nil {
		return 0
	}

	total, err := strconv.ParseInt(totalS, 10, 64)
	if err != nil {
		// represents a corrupted file, should probably remove.
		return 0
	}

	return total
}

// Writer begins or resumes the active writer identified by ref. If the writer
// is already in use, an error is returned. Only one writer may be in use per
// ref at a time.
//
// The argument `ref` is used to uniquely identify a long-lived writer transaction.
func (s *store) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	var wOpts content.WriterOpts
	for _, opt := range opts {
		if err := opt(&wOpts); err != nil {
			return nil, err
		}
	}
	// TODO(AkihiroSuda): we could create a random string or one calculated based on the context
	// https://github.com/containerd/containerd/issues/2129#issuecomment-380255019
	if wOpts.Ref == "" {
		return nil, errors.Wrap(errdefs.ErrInvalidArgument, "ref must not be empty")
	}
	var lockErr error
	for count := uint64(0); count < 10; count++ {
		time.Sleep(time.Millisecond * time.Duration(rand.Intn(1<<count)))
		if err := tryLock(wOpts.Ref); err != nil {
			if !errdefs.IsUnavailable(err) {
				return nil, err
			}

			lockErr = err
		} else {
			lockErr = nil
			break
		}
	}

	if lockErr != nil {
		return nil, lockErr
	}

	w, err := s.writer(ctx, wOpts.Ref, wOpts.Desc.Size, wOpts.Desc.Digest)
	if err != nil {
		unlock(wOpts.Ref)
		return nil, err
	}

	return w, nil // lock is now held by w.
}

func (s *store) resumeStatus(ref string, total int64, digester digest.Digester) (content.Status, error) {
	path, _, data := s.ingestPaths(ref)
	status, err := s.status(path)
	if err != nil {
		return status, errors.Wrap(err, "failed reading status of resume write")
	}
	if ref != status.Ref {
		// NOTE(stevvooe): This is fairly catastrophic. Either we have some
		// layout corruption or a hash collision for the ref key.
		return status, errors.Wrapf(err, "ref key does not match: %v != %v", ref, status.Ref)
	}

	if total > 0 && status.Total > 0 && total != status.Total {
		return status, errors.Errorf("provided total differs from status: %v != %v", total, status.Total)
	}

	// TODO(stevvooe): slow slow slow!!, send to goroutine or use resumable hashes
	fp, err := os.Open(data)
	if err != nil {
		return status, err
	}

	p := bufPool.Get().(*[]byte)
	status.Offset, err = io.CopyBuffer(digester.Hash(), fp, *p)
	bufPool.Put(p)
	fp.Close()
	return status, err
}

// writer provides the main implementation of the Writer method. The caller
// must hold the lock correctly and release on error if there is a problem.
func (s *store) writer(ctx context.Context, ref string, total int64, expected digest.Digest) (content.Writer, error) {
	// TODO(stevvooe): Need to actually store expected here. We have
	// code in the service that shouldn't be dealing with this.
	if expected != "" {
		p := s.blobPath(expected)
		if _, err := os.Stat(p); err == nil {
			return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "content %v", expected)
		}
	}

	path, refp, data := s.ingestPaths(ref)

	var (
		digester  = digest.Canonical.Digester()
		offset    int64
		startedAt time.Time
		updatedAt time.Time
	)

	foundValidIngest := false
	// ensure that the ingest path has been created.
	if err := os.Mkdir(path, 0755); err != nil {
		if !os.IsExist(err) {
			return nil, err
		}
		status, err := s.resumeStatus(ref, total, digester)
		if err == nil {
			foundValidIngest = true
			updatedAt = status.UpdatedAt
			startedAt = status.StartedAt
			total = status.Total
			offset = status.Offset
		} else {
			logrus.Infof("failed to resume the status from path %s: %s. will recreate them", path, err.Error())
		}
	}

	if !foundValidIngest {
		startedAt = time.Now()
		updatedAt = startedAt

		// the ingest is new, we need to setup the target location.
		// write the ref to a file for later use
		if err := ioutil.WriteFile(refp, []byte(ref), 0666); err != nil {
			return nil, err
		}

		if err := writeTimestampFile(filepath.Join(path, "startedat"), startedAt); err != nil {
			return nil, err
		}

		if err := writeTimestampFile(filepath.Join(path, "updatedat"), startedAt); err != nil {
			return nil, err
		}

		if total > 0 {
			if err := ioutil.WriteFile(filepath.Join(path, "total"), []byte(fmt.Sprint(total)), 0666); err != nil {
				return nil, err
			}
		}
	}

	fp, err := os.OpenFile(data, os.O_WRONLY|os.O_CREATE, 0666)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open data file")
	}

	if _, err := fp.Seek(offset, io.SeekStart); err != nil {
		return nil, errors.Wrap(err, "could not seek to current write offset")
	}

	return &writer{
		s:         s,
		fp:        fp,
		ref:       ref,
		path:      path,
		offset:    offset,
		total:     total,
		digester:  digester,
		startedAt: startedAt,
		updatedAt: updatedAt,
	}, nil
}

// Abort an active transaction keyed by ref. If the ingest is active, it will
// be cancelled. Any resources associated with the ingest will be cleaned.
func (s *store) Abort(ctx context.Context, ref string) error {
	root := s.ingestRoot(ref)
	if err := os.RemoveAll(root); err != nil {
		if os.IsNotExist(err) {
			return errors.Wrapf(errdefs.ErrNotFound, "ingest ref %q", ref)
		}

		return err
	}

	return nil
}

func (s *store) blobPath(dgst digest.Digest) string {
	return filepath.Join(s.root, "blobs", dgst.Algorithm().String(), dgst.Hex())
}

func (s *store) ingestRoot(ref string) string {
	dgst := digest.FromString(ref)
	return filepath.Join(s.root, "ingest", dgst.Hex())
}

// ingestPaths are returned. The paths are the following:
//
// - root: entire ingest directory
// - ref: name of the starting ref, must be unique
// - data: file where data is written
//
func (s *store) ingestPaths(ref string) (string, string, string) {
	var (
		fp = s.ingestRoot(ref)
		rp = filepath.Join(fp, "ref")
		dp = filepath.Join(fp, "data")
	)

	return fp, rp, dp
}

func readFileString(path string) (string, error) {
	p, err := ioutil.ReadFile(path)
	return string(p), err
}

// readFileTimestamp reads a file with just a timestamp present.
func readFileTimestamp(p string) (time.Time, error) {
	b, err := ioutil.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			err = errors.Wrap(errdefs.ErrNotFound, err.Error())
		}
		return time.Time{}, err
	}

	var t time.Time
	if err := t.UnmarshalText(b); err != nil {
		return time.Time{}, errors.Wrapf(err, "could not parse timestamp file %v", p)
	}

	return t, nil
}

func writeTimestampFile(p string, t time.Time) error {
	b, err := t.MarshalText()
	if err != nil {
		return err
	}

	return continuity.AtomicWriteFile(p, b, 0666)
}
