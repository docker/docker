package store

import (
	"strings"

	"github.com/docker/swarmkit/api"
	memdb "github.com/hashicorp/go-memdb"
)

const tableVolume = "volume"

func init() {
	register(ObjectStoreConfig{
		Table: &memdb.TableSchema{
			Name: tableVolume,
			Indexes: map[string]*memdb.IndexSchema{
				indexID: {
					Name:    indexID,
					Unique:  true,
					Indexer: api.VolumeIndexerByID{},
				},
				indexName: {
					Name:    indexName,
					Unique:  true,
					Indexer: api.VolumeIndexerByName{},
				},
				indexCustom: {
					Name:         indexCustom,
					Indexer:      api.VolumeCustomIndexer{},
					AllowMissing: true,
				},
				indexVolumeGroup: {
					Name:    indexVolumeGroup,
					Indexer: volumeIndexerByGroup{},
				},
				indexDriver: {
					Name:    indexDriver,
					Indexer: volumeIndexerByDriver{},
				},
			},
		},
		Save: func(tx ReadTx, snapshot *api.StoreSnapshot) error {
			var err error
			snapshot.Volumes, err = FindVolumes(tx, All)
			return err
		},
		Restore: func(tx Tx, snapshot *api.StoreSnapshot) error {
			toStoreObj := make([]api.StoreObject, len(snapshot.Volumes))
			for i, x := range snapshot.Volumes {
				toStoreObj[i] = x
			}
			return RestoreTable(tx, tableVolume, toStoreObj)
		},
		ApplyStoreAction: func(tx Tx, sa api.StoreAction) error {
			switch v := sa.Target.(type) {
			case *api.StoreAction_Volume:
				obj := v.Volume
				switch sa.Action {
				case api.StoreActionKindCreate:
					return CreateVolume(tx, obj)
				case api.StoreActionKindUpdate:
					return UpdateVolume(tx, obj)
				case api.StoreActionKindRemove:
					return DeleteVolume(tx, obj.ID)
				}
			}
			return errUnknownStoreAction
		},
	})
}

func CreateVolume(tx Tx, v *api.Volume) error {
	if tx.lookup(tableVolume, indexName, strings.ToLower(v.Spec.Annotations.Name)) != nil {
		return ErrNameConflict
	}

	return tx.create(tableVolume, v)
}

func UpdateVolume(tx Tx, v *api.Volume) error {
	// ensure the name is either not in use, or is in use by this volume.
	if existing := tx.lookup(tableVolume, indexName, strings.ToLower(v.Spec.Annotations.Name)); existing != nil {
		if existing.GetID() != v.ID {
			return ErrNameConflict
		}
	}

	return tx.update(tableVolume, v)
}

func DeleteVolume(tx Tx, id string) error {
	return tx.delete(tableVolume, id)
}

func GetVolume(tx ReadTx, id string) *api.Volume {
	n := tx.get(tableVolume, id)
	if n == nil {
		return nil
	}
	return n.(*api.Volume)
}

func FindVolumes(tx ReadTx, by By) ([]*api.Volume, error) {
	checkType := func(by By) error {
		switch by.(type) {
		case byName, byNamePrefix, byIDPrefix, byVolumeGroup, byCustom, byCustomPrefix, byDriver:
			return nil
		default:
			return ErrInvalidFindBy
		}
	}

	volumeList := []*api.Volume{}
	appendResult := func(o api.StoreObject) {
		volumeList = append(volumeList, o.(*api.Volume))
	}

	err := tx.find(tableVolume, by, checkType, appendResult)
	return volumeList, err
}

type volumeIndexerByGroup struct{}

func (vi volumeIndexerByGroup) FromArgs(args ...interface{}) ([]byte, error) {
	return fromArgs(args...)
}

func (vi volumeIndexerByGroup) FromObject(obj interface{}) (bool, []byte, error) {
	v := obj.(*api.Volume)
	val := v.Spec.Group + "\x00"
	return true, []byte(val), nil
}

type volumeIndexerByDriver struct{}

func (vi volumeIndexerByDriver) FromArgs(args ...interface{}) ([]byte, error) {
	return fromArgs(args...)
}

func (vi volumeIndexerByDriver) FromObject(obj interface{}) (bool, []byte, error) {
	v := obj.(*api.Volume)
	// this should never happen -- existence of the volume driver is checked
	// at the controlapi level. However, guard against the unforeseen.
	if v.Spec.Driver == nil {
		return false, nil, nil
	}
	val := v.Spec.Driver.Name + "\x00"
	return true, []byte(val), nil
}
