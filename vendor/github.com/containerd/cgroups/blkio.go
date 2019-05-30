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

package cgroups

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func NewBlkio(root string) *blkioController {
	return &blkioController{
		root: filepath.Join(root, string(Blkio)),
	}
}

type blkioController struct {
	root string
}

func (b *blkioController) Name() Name {
	return Blkio
}

func (b *blkioController) Path(path string) string {
	return filepath.Join(b.root, path)
}

func (b *blkioController) Create(path string, resources *specs.LinuxResources) error {
	if err := os.MkdirAll(b.Path(path), defaultDirPerm); err != nil {
		return err
	}
	if resources.BlockIO == nil {
		return nil
	}
	for _, t := range createBlkioSettings(resources.BlockIO) {
		if t.value != nil {
			if err := ioutil.WriteFile(
				filepath.Join(b.Path(path), fmt.Sprintf("blkio.%s", t.name)),
				t.format(t.value),
				defaultFilePerm,
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *blkioController) Update(path string, resources *specs.LinuxResources) error {
	return b.Create(path, resources)
}

func (b *blkioController) Stat(path string, stats *Metrics) error {
	stats.Blkio = &BlkIOStat{}
	settings := []blkioStatSettings{
		{
			name:  "throttle.io_serviced",
			entry: &stats.Blkio.IoServicedRecursive,
		},
		{
			name:  "throttle.io_service_bytes",
			entry: &stats.Blkio.IoServiceBytesRecursive,
		},
	}
	// Try to read CFQ stats available on all CFQ enabled kernels first
	if _, err := os.Lstat(filepath.Join(b.Path(path), fmt.Sprintf("blkio.io_serviced_recursive"))); err == nil {
		settings = append(settings,
			blkioStatSettings{
				name:  "sectors_recursive",
				entry: &stats.Blkio.SectorsRecursive,
			},
			blkioStatSettings{
				name:  "io_service_bytes_recursive",
				entry: &stats.Blkio.IoServiceBytesRecursive,
			},
			blkioStatSettings{
				name:  "io_serviced_recursive",
				entry: &stats.Blkio.IoServicedRecursive,
			},
			blkioStatSettings{
				name:  "io_queued_recursive",
				entry: &stats.Blkio.IoQueuedRecursive,
			},
			blkioStatSettings{
				name:  "io_service_time_recursive",
				entry: &stats.Blkio.IoServiceTimeRecursive,
			},
			blkioStatSettings{
				name:  "io_wait_time_recursive",
				entry: &stats.Blkio.IoWaitTimeRecursive,
			},
			blkioStatSettings{
				name:  "io_merged_recursive",
				entry: &stats.Blkio.IoMergedRecursive,
			},
			blkioStatSettings{
				name:  "time_recursive",
				entry: &stats.Blkio.IoTimeRecursive,
			},
		)
	}
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return err
	}
	defer f.Close()

	devices, err := getDevices(f)
	if err != nil {
		return err
	}

	for _, t := range settings {
		if err := b.readEntry(devices, path, t.name, t.entry); err != nil {
			return err
		}
	}
	return nil
}

func (b *blkioController) readEntry(devices map[deviceKey]string, path, name string, entry *[]*BlkIOEntry) error {
	f, err := os.Open(filepath.Join(b.Path(path), fmt.Sprintf("blkio.%s", name)))
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if err := sc.Err(); err != nil {
			return err
		}
		// format: dev type amount
		fields := strings.FieldsFunc(sc.Text(), splitBlkIOStatLine)
		if len(fields) < 3 {
			if len(fields) == 2 && fields[0] == "Total" {
				// skip total line
				continue
			} else {
				return fmt.Errorf("Invalid line found while parsing %s: %s", path, sc.Text())
			}
		}
		major, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return err
		}
		minor, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return err
		}
		op := ""
		valueField := 2
		if len(fields) == 4 {
			op = fields[2]
			valueField = 3
		}
		v, err := strconv.ParseUint(fields[valueField], 10, 64)
		if err != nil {
			return err
		}
		*entry = append(*entry, &BlkIOEntry{
			Device: devices[deviceKey{major, minor}],
			Major:  major,
			Minor:  minor,
			Op:     op,
			Value:  v,
		})
	}
	return nil
}

func createBlkioSettings(blkio *specs.LinuxBlockIO) []blkioSettings {
	settings := []blkioSettings{}

	if blkio.Weight != nil {
		settings = append(settings,
			blkioSettings{
				name:   "weight",
				value:  blkio.Weight,
				format: uintf,
			})
	}
	if blkio.LeafWeight != nil {
		settings = append(settings,
			blkioSettings{
				name:   "leaf_weight",
				value:  blkio.LeafWeight,
				format: uintf,
			})
	}
	for _, wd := range blkio.WeightDevice {
		if wd.Weight != nil {
			settings = append(settings,
				blkioSettings{
					name:   "weight_device",
					value:  wd,
					format: weightdev,
				})
		}
		if wd.LeafWeight != nil {
			settings = append(settings,
				blkioSettings{
					name:   "leaf_weight_device",
					value:  wd,
					format: weightleafdev,
				})
		}
	}
	for _, t := range []struct {
		name string
		list []specs.LinuxThrottleDevice
	}{
		{
			name: "throttle.read_bps_device",
			list: blkio.ThrottleReadBpsDevice,
		},
		{
			name: "throttle.read_iops_device",
			list: blkio.ThrottleReadIOPSDevice,
		},
		{
			name: "throttle.write_bps_device",
			list: blkio.ThrottleWriteBpsDevice,
		},
		{
			name: "throttle.write_iops_device",
			list: blkio.ThrottleWriteIOPSDevice,
		},
	} {
		for _, td := range t.list {
			settings = append(settings, blkioSettings{
				name:   t.name,
				value:  td,
				format: throttleddev,
			})
		}
	}
	return settings
}

type blkioSettings struct {
	name   string
	value  interface{}
	format func(v interface{}) []byte
}

type blkioStatSettings struct {
	name  string
	entry *[]*BlkIOEntry
}

func uintf(v interface{}) []byte {
	return []byte(strconv.FormatUint(uint64(*v.(*uint16)), 10))
}

func weightdev(v interface{}) []byte {
	wd := v.(specs.LinuxWeightDevice)
	return []byte(fmt.Sprintf("%d:%d %d", wd.Major, wd.Minor, *wd.Weight))
}

func weightleafdev(v interface{}) []byte {
	wd := v.(specs.LinuxWeightDevice)
	return []byte(fmt.Sprintf("%d:%d %d", wd.Major, wd.Minor, *wd.LeafWeight))
}

func throttleddev(v interface{}) []byte {
	td := v.(specs.LinuxThrottleDevice)
	return []byte(fmt.Sprintf("%d:%d %d", td.Major, td.Minor, td.Rate))
}

func splitBlkIOStatLine(r rune) bool {
	return r == ' ' || r == ':'
}

type deviceKey struct {
	major, minor uint64
}

// getDevices makes a best effort attempt to read all the devices into a map
// keyed by major and minor number. Since devices may be mapped multiple times,
// we err on taking the first occurrence.
func getDevices(r io.Reader) (map[deviceKey]string, error) {

	var (
		s       = bufio.NewScanner(r)
		devices = make(map[deviceKey]string)
	)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		major, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, err
		}
		minor, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		key := deviceKey{
			major: uint64(major),
			minor: uint64(minor),
		}
		if _, ok := devices[key]; ok {
			continue
		}
		devices[key] = filepath.Join("/dev", fields[2])
	}
	return devices, s.Err()
}

func major(devNumber uint64) uint64 {
	return (devNumber >> 8) & 0xfff
}

func minor(devNumber uint64) uint64 {
	return (devNumber & 0xff) | ((devNumber >> 12) & 0xfff00)
}
