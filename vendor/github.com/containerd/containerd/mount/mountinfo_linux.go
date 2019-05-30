// +build linux

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

package mount

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

// Self retrieves a list of mounts for the current running process.
func Self() ([]Info, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return parseInfoFile(f)
}

func parseInfoFile(r io.Reader) ([]Info, error) {
	s := bufio.NewScanner(r)
	out := []Info{}
	var err error
	for s.Scan() {
		if err = s.Err(); err != nil {
			return nil, err
		}

		/*
		   See http://man7.org/linux/man-pages/man5/proc.5.html

		   36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw,errors=continue
		   (1)(2)(3)   (4)   (5)      (6)      (7)   (8) (9)   (10)         (11)
		   (1) mount ID:  unique identifier of the mount (may be reused after umount)
		   (2) parent ID:  ID of parent (or of self for the top of the mount tree)
		   (3) major:minor:  value of st_dev for files on filesystem
		   (4) root:  root of the mount within the filesystem
		   (5) mount point:  mount point relative to the process's root
		   (6) mount options:  per mount options
		   (7) optional fields:  zero or more fields of the form "tag[:value]"
		   (8) separator:  marks the end of the optional fields
		   (9) filesystem type:  name of filesystem of the form "type[.subtype]"
		   (10) mount source:  filesystem specific information or "none"
		   (11) super options:  per super block options
		*/

		text := s.Text()
		fields := strings.Split(text, " ")
		numFields := len(fields)
		if numFields < 10 {
			// should be at least 10 fields
			return nil, errors.Errorf("parsing '%s' failed: not enough fields (%d)", text, numFields)
		}
		p := Info{}
		// ignore any numbers parsing errors, as there should not be any
		p.ID, _ = strconv.Atoi(fields[0])
		p.Parent, _ = strconv.Atoi(fields[1])
		mm := strings.Split(fields[2], ":")
		if len(mm) != 2 {
			return nil, errors.Errorf("parsing '%s' failed: unexpected minor:major pair %s", text, mm)
		}
		p.Major, _ = strconv.Atoi(mm[0])
		p.Minor, _ = strconv.Atoi(mm[1])

		p.Root, err = strconv.Unquote(`"` + fields[3] + `"`)
		if err != nil {
			return nil, errors.Wrapf(err, "parsing '%s' failed: unable to unquote root field", fields[3])
		}
		p.Mountpoint, err = strconv.Unquote(`"` + fields[4] + `"`)
		if err != nil {
			return nil, errors.Wrapf(err, "parsing '%s' failed: unable to unquote mount point field", fields[4])
		}
		p.Options = fields[5]

		// one or more optional fields, when a separator (-)
		i := 6
		for ; i < numFields && fields[i] != "-"; i++ {
			switch i {
			case 6:
				p.Optional = fields[6]
			default:
				/* NOTE there might be more optional fields before the separator
				   such as fields[7]...fields[N] (where N < separatorIndex),
				   although as of Linux kernel 4.15 the only known ones are
				   mount propagation flags in fields[6]. The correct
				   behavior is to ignore any unknown optional fields.
				*/
			}
		}
		if i == numFields {
			return nil, errors.Errorf("parsing '%s' failed: missing separator ('-')", text)
		}
		// There should be 3 fields after the separator...
		if i+4 > numFields {
			return nil, errors.Errorf("parsing '%s' failed: not enough fields after a separator", text)
		}
		// ... but in Linux <= 3.9 mounting a cifs with spaces in a share name
		// (like "//serv/My Documents") _may_ end up having a space in the last field
		// of mountinfo (like "unc=//serv/My Documents"). Since kernel 3.10-rc1, cifs
		// option unc= is ignored,  so a space should not appear. In here we ignore
		// those "extra" fields caused by extra spaces.
		p.FSType = fields[i+1]
		p.Source = fields[i+2]
		p.VFSOptions = fields[i+3]

		out = append(out, p)
	}
	return out, nil
}

// PID collects the mounts for a specific process ID. If the process
// ID is unknown, it is better to use `Self` which will inspect
// "/proc/self/mountinfo" instead.
func PID(pid int) ([]Info, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mountinfo", pid))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return parseInfoFile(f)
}
