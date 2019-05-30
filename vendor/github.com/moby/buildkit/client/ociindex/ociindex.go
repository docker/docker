package ociindex

import (
	"encoding/json"
	"io/ioutil"
	"os"

	"github.com/gofrs/flock"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	// IndexJSONLockFileSuffix is the suffix of the lock file
	IndexJSONLockFileSuffix = ".lock"
)

// PutDescToIndex puts desc to index with tag.
// Existing manifests with the same tag will be removed from the index.
func PutDescToIndex(index *v1.Index, desc v1.Descriptor, tag string) error {
	if index == nil {
		index = &v1.Index{}
	}
	if index.SchemaVersion == 0 {
		index.SchemaVersion = 2
	}
	if tag != "" {
		if desc.Annotations == nil {
			desc.Annotations = make(map[string]string)
		}
		desc.Annotations[v1.AnnotationRefName] = tag
		// remove existing manifests with the same tag
		var manifests []v1.Descriptor
		for _, m := range index.Manifests {
			if m.Annotations[v1.AnnotationRefName] != tag {
				manifests = append(manifests, m)
			}
		}
		index.Manifests = manifests
	}
	index.Manifests = append(index.Manifests, desc)
	return nil
}

func PutDescToIndexJSONFileLocked(indexJSONPath string, desc v1.Descriptor, tag string) error {
	lockPath := indexJSONPath + IndexJSONLockFileSuffix
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return errors.Wrapf(err, "could not lock %s", lockPath)
	}
	if !locked {
		return errors.Errorf("could not lock %s", lockPath)
	}
	defer func() {
		lock.Unlock()
		os.RemoveAll(lockPath)
	}()
	f, err := os.OpenFile(indexJSONPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return errors.Wrapf(err, "could not open %s", indexJSONPath)
	}
	defer f.Close()
	var idx v1.Index
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return errors.Wrapf(err, "could not read %s", indexJSONPath)
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &idx); err != nil {
			return errors.Wrapf(err, "could not unmarshal %s (%q)", indexJSONPath, string(b))
		}
	}
	if err = PutDescToIndex(&idx, desc, tag); err != nil {
		return err
	}
	b, err = json.Marshal(idx)
	if err != nil {
		return err
	}
	if _, err = f.WriteAt(b, 0); err != nil {
		return err
	}
	if err = f.Truncate(int64(len(b))); err != nil {
		return err
	}
	return nil
}

func ReadIndexJSONFileLocked(indexJSONPath string) (*v1.Index, error) {
	lockPath := indexJSONPath + IndexJSONLockFileSuffix
	lock := flock.New(lockPath)
	locked, err := lock.TryRLock()
	if err != nil {
		return nil, errors.Wrapf(err, "could not lock %s", lockPath)
	}
	if !locked {
		return nil, errors.Errorf("could not lock %s", lockPath)
	}
	defer func() {
		lock.Unlock()
		os.RemoveAll(lockPath)
	}()
	b, err := ioutil.ReadFile(indexJSONPath)
	if err != nil {
		return nil, errors.Wrapf(err, "could not read %s", indexJSONPath)
	}
	var idx v1.Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, errors.Wrapf(err, "could not unmarshal %s (%q)", indexJSONPath, string(b))
	}
	return &idx, nil
}
