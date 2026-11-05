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

/*
   This file is copied and customized based on
   https://github.com/moby/moby/blob/master/pkg/idtools/idtools.go
*/

package userns

import (
	"errors"
	"fmt"
	"strings"

	"github.com/opencontainers/runtime-spec/specs-go"
)

const invalidID = 1<<32 - 1

var invalidUser = User{Uid: invalidID, Gid: invalidID}

// User is a Uid and Gid pair of a user
//
//nolint:revive,staticcheck // ST1003: struct field Uid should be UID (staticcheck)
type User struct {
	Uid uint32
	Gid uint32
}

// IDMap contains the mappings of Uids and Gids.
//
//nolint:revive,staticcheck // ST1003: struct field Uid should be UID (staticcheck)
type IDMap struct {
	UidMap []specs.LinuxIDMapping `json:"UidMap"`
	GidMap []specs.LinuxIDMapping `json:"GidMap"`
}

// RootPair returns the ID pair for the root user
func (i *IDMap) RootPair() (User, error) {
	uid, err := toHost(0, i.UidMap)
	if err != nil {
		return invalidUser, err
	}
	gid, err := toHost(0, i.GidMap)
	if err != nil {
		return invalidUser, err
	}
	return User{Uid: uid, Gid: gid}, nil
}

// ToHost returns the host user ID pair for the container ID pair.
func (i *IDMap) ToHost(pair User) (User, error) {
	var (
		target User
		err    error
	)
	target.Uid, err = toHost(pair.Uid, i.UidMap)
	if err != nil {
		return invalidUser, err
	}
	target.Gid, err = toHost(pair.Gid, i.GidMap)
	if err != nil {
		return invalidUser, err
	}
	return target, nil
}

// ToContainer returns the container user ID pair for the host ID pair.
func (i *IDMap) ToContainer(pair User) (User, error) {
	var (
		target User
		err    error
	)
	target.Uid, err = toContainer(pair.Uid, i.UidMap)
	if err != nil {
		return invalidUser, err
	}
	target.Gid, err = toContainer(pair.Gid, i.GidMap)
	if err != nil {
		return invalidUser, err
	}
	return target, nil
}

// Marshal serializes the IDMap object into two strings:
// one uidmap list and another one for gidmap list
func (i *IDMap) Marshal() (string, string) {
	marshal := func(mappings []specs.LinuxIDMapping) string {
		var arr []string
		for _, m := range mappings {
			arr = append(arr, serializeLinuxIDMapping(m))
		}
		return strings.Join(arr, ",")
	}
	return marshal(i.UidMap), marshal(i.GidMap)
}

// Unmarshal deserialize the passed uidmap and gidmap strings
// into a IDMap object. Error is returned in case of failure
func (i *IDMap) Unmarshal(uidMap, gidMap string) error {
	unmarshal := func(str string, fn func(m specs.LinuxIDMapping)) error {
		if len(str) == 0 {
			return nil
		}
		for mapping := range strings.SplitSeq(str, ",") {
			m, err := deserializeLinuxIDMapping(mapping)
			if err != nil {
				return err
			}
			fn(m)
		}
		return nil
	}
	if err := unmarshal(uidMap, func(m specs.LinuxIDMapping) {
		i.UidMap = append(i.UidMap, m)
	}); err != nil {
		return err
	}
	return unmarshal(gidMap, func(m specs.LinuxIDMapping) {
		i.GidMap = append(i.GidMap, m)
	})
}

// toHost takes an id mapping and a remapped ID, and translates the
// ID to the mapped host ID. If no map is provided, then the translation
// assumes a 1-to-1 mapping and returns the passed in id #
func toHost(contID uint32, idMap []specs.LinuxIDMapping) (uint32, error) {
	if idMap == nil {
		return contID, nil
	}
	for _, m := range idMap {
		high, err := safeSum(m.ContainerID, m.Size)
		if err != nil {
			break
		}
		if contID >= m.ContainerID && contID < high {
			hostID, err := safeSum(m.HostID, contID-m.ContainerID)
			if err != nil || hostID == invalidID {
				break
			}
			return hostID, nil
		}
	}
	return invalidID, fmt.Errorf("container ID %d cannot be mapped to a host ID", contID)
}

// toContainer takes an id mapping and a host ID, and translates the ID to the
// mapped container ID. If no map is provided, then the translation assumes a
// 1-to-1 mapping and returns the passed in id #
func toContainer(hostID uint32, idMap []specs.LinuxIDMapping) (uint32, error) {
	if idMap == nil {
		return hostID, nil
	}
	for _, m := range idMap {
		high, err := safeSum(m.HostID, m.Size)
		if err != nil {
			break
		}
		if hostID >= m.HostID && hostID < high {
			contID, err := safeSum(m.ContainerID, hostID-m.HostID)
			if err != nil || contID == invalidID {
				break
			}
			return contID, nil
		}
	}
	return invalidID, fmt.Errorf("host ID %d cannot be mapped to a container ID", hostID)
}

// safeSum returns the sum of x and y. or an error if the result overflows
func safeSum(x, y uint32) (uint32, error) {
	z := x + y
	if z < x || z < y {
		return invalidID, errors.New("ID overflow")
	}
	return z, nil
}

// serializeLinuxIDMapping marshals a LinuxIDMapping object to string
func serializeLinuxIDMapping(m specs.LinuxIDMapping) string {
	return fmt.Sprintf("%d:%d:%d", m.ContainerID, m.HostID, m.Size)
}

// deserializeLinuxIDMapping unmarshals a string to a LinuxIDMapping object
func deserializeLinuxIDMapping(str string) (specs.LinuxIDMapping, error) {
	var (
		hostID, ctrID, length int64
	)
	_, err := fmt.Sscanf(str, "%d:%d:%d", &ctrID, &hostID, &length)
	if err != nil {
		return specs.LinuxIDMapping{}, fmt.Errorf("input value %s unparsable: %w", str, err)
	}
	if ctrID < 0 || ctrID >= invalidID || hostID < 0 || hostID >= invalidID || length < 0 || length >= invalidID {
		return specs.LinuxIDMapping{}, fmt.Errorf("invalid mapping \"%s\"", str)
	}
	return specs.LinuxIDMapping{
		ContainerID: uint32(ctrID),
		HostID:      uint32(hostID),
		Size:        uint32(length),
	}, nil
}
