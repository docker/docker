//go:build !windows

package network

import (
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"

	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/filters"
)

var _ FilterNetwork = mockFilterNetwork{}

type mockFilterNetwork struct {
	driver     string
	name       string
	id         string
	labels     map[string]string
	scope      string
	created    time.Time
	containers bool
	services   bool
}

func (n mockFilterNetwork) Driver() string {
	return n.driver
}

func (n mockFilterNetwork) Name() string {
	return n.name
}

func (n mockFilterNetwork) ID() string {
	return n.id
}

func (n mockFilterNetwork) Labels() map[string]string {
	return n.labels
}

func (n mockFilterNetwork) Scope() string {
	return n.scope
}

func (n mockFilterNetwork) Created() time.Time {
	return n.created
}

func (n mockFilterNetwork) HasContainerAttachments() bool {
	return n.containers
}

func (n mockFilterNetwork) HasServiceAttachments() bool {
	return n.services
}

func TestFilter(t *testing.T) {
	networks := []mockFilterNetwork{
		{
			name:    network.NetworkHost,
			id:      "ubfg", // ROT13(name)
			driver:  "host",
			scope:   "local",
			created: time.Date(2025, time.June, 1, 0, 0, 0, 0, time.Local),
		},
		{
			name:    network.NetworkBridge,
			id:      "oevqtr",
			driver:  "bridge",
			scope:   "local",
			created: time.Date(2025, time.June, 1, 0, 0, 0, 0, time.Local),
		},
		{
			name:    network.NetworkNone,
			id:      "abar",
			driver:  "null",
			scope:   "local",
			created: time.Date(2025, time.June, 1, 0, 0, 0, 0, time.Local),
		},
		{
			name:    "myoverlay",
			id:      "zlbireynl",
			driver:  "overlay",
			scope:   "swarm",
			created: time.Date(2024, time.June, 1, 12, 0, 0, 0, time.Local),
		},
		{
			name:    "mydrivernet",
			id:      "zlqevirearg",
			driver:  "mydriver",
			scope:   "local",
			labels:  map[string]string{"foo": "bar"},
			created: time.Date(2024, time.December, 1, 2, 0, 0, 0, time.Local),
		},
		{
			name:    "mykvnet",
			id:      "zlxiarg",
			driver:  "mykvdriver",
			scope:   "global",
			created: time.Date(2025, time.January, 1, 0, 0, 0, 0, time.Local),
		},
		{
			name:       "networkwithcontainer",
			id:         "argjbexjvgupbagnvare",
			driver:     "nwc",
			scope:      "local",
			containers: true,
			created:    time.Date(2025, time.June, 1, 0, 0, 0, 0, time.Local),
		},
		{
			name:     "networkwithservice",
			id:       "argjbexjvgufreivpr",
			driver:   "nwc",
			scope:    "local",
			services: true,
			created:  time.Date(2025, time.June, 1, 0, 0, 0, 0, time.Local),
		},
		{
			name:    "idoverlap",
			id:      "aaaaa0my0bbbbbb",
			driver:  "nwc",
			scope:   "local",
			created: time.Date(2025, time.February, 1, 0, 0, 0, 0, time.Local),
		},
	}

	testCases := []struct {
		subtest           string
		filter            filters.Args
		pruneFilter       bool
		idAlsoMatchesName bool

		err     string
		results []string
	}{
		{
			subtest: "EmptyFilter",
			filter:  filters.NewArgs(),
			results: (func() []string {
				var r []string
				for _, n := range networks {
					r = append(r, n.name)
				}
				return r
			})(),
		},
		{
			subtest: "driver=bridge",
			filter:  filters.NewArgs(filters.Arg("driver", "bridge")),
			results: []string{"bridge"},
		},
		{
			subtest: "driver=overlay",
			filter:  filters.NewArgs(filters.Arg("driver", "overlay")),
			results: []string{"myoverlay"},
		},
		{
			subtest: "driver=noname",
			filter:  filters.NewArgs(filters.Arg("driver", "noname")),
		},
		{
			subtest: "type=custom",
			filter:  filters.NewArgs(filters.Arg("type", "custom")),
			results: []string{"myoverlay", "mydrivernet", "mykvnet", "networkwithcontainer", "networkwithservice", "idoverlap"},
		},
		{
			subtest: "type=builtin",
			filter:  filters.NewArgs(filters.Arg("type", "builtin")),
			results: []string{network.NetworkHost, network.NetworkBridge, network.NetworkNone},
		},
		{
			subtest: "type=builtin,type=custom",
			filter:  filters.NewArgs(filters.Arg("type", "builtin"), filters.Arg("type", "custom")),
			results: []string{network.NetworkHost, network.NetworkBridge, network.NetworkNone, "myoverlay", "mydrivernet", "mykvnet", "networkwithcontainer", "networkwithservice", "idoverlap"},
		},
		{
			subtest: "type=invalid",
			filter:  filters.NewArgs(filters.Arg("type", "invalid")),
			err:     "invalid value for filter 'type': must be 'builtin' or 'custom'",
		},
		{
			subtest: "scope=local",
			filter:  filters.NewArgs(filters.Arg("scope", "local")),
			results: []string{network.NetworkHost, network.NetworkBridge, network.NetworkNone, "mydrivernet", "networkwithcontainer", "networkwithservice", "idoverlap"},
		},
		{
			subtest: "scope=swarm",
			filter:  filters.NewArgs(filters.Arg("scope", "swarm")),
			results: []string{"myoverlay"},
		},
		{
			subtest: "scope=global",
			filter:  filters.NewArgs(filters.Arg("scope", "global")),
			results: []string{"mykvnet"},
		},
		{
			subtest: "dangling=true",
			filter:  filters.NewArgs(filters.Arg("dangling", "true")),
			results: []string{"myoverlay", "mydrivernet", "mykvnet", "idoverlap"},
		},
		{
			subtest: "dangling=1",
			filter:  filters.NewArgs(filters.Arg("dangling", "1")),
			results: []string{"myoverlay", "mydrivernet", "mykvnet", "idoverlap"},
		},
		{
			subtest: "dangling=false",
			filter:  filters.NewArgs(filters.Arg("dangling", "false")),
			results: []string{network.NetworkHost, network.NetworkBridge, network.NetworkNone, "networkwithcontainer", "networkwithservice"},
		},
		{
			subtest: "dangling=0",
			filter:  filters.NewArgs(filters.Arg("dangling", "0")),
			results: []string{network.NetworkHost, network.NetworkBridge, network.NetworkNone, "networkwithcontainer", "networkwithservice"},
		},
		{
			subtest: "dangling=asdf",
			filter:  filters.NewArgs(filters.Arg("dangling", "asdf")),
			err:     "invalid value for filter 'dangling'",
		},
		{
			subtest: "MultipleTerms=dangling",
			filter:  filters.NewArgs(filters.Arg("dangling", "1"), filters.Arg("dangling", "true")),
			err:     `got more than one value for filter key "dangling"`,
		},
		{
			subtest: "label=foo",
			filter:  filters.NewArgs(filters.Arg("label", "foo")),
			results: []string{"mydrivernet"},
		},
		{
			subtest: "label=foo=bar",
			filter:  filters.NewArgs(filters.Arg("label", "foo=bar")),
			results: []string{"mydrivernet"},
		},
		{
			subtest: "label=foo=baz",
			filter:  filters.NewArgs(filters.Arg("label", "foo=baz")),
		},
		{
			subtest: "label!=foo",
			filter:  filters.NewArgs(filters.Arg("label!", "foo")),
			err:     "invalid filter 'label!'",
		},
		{
			subtest: "until=1h",
			filter:  filters.NewArgs(filters.Arg("until", "1h")),
			err:     "invalid filter 'until'",
		},
		{
			subtest: "name=with",
			filter:  filters.NewArgs(filters.Arg("name", "with")),
			results: []string{"networkwithcontainer", "networkwithservice"},
		},
		{
			subtest: "id=with/IDAlsoMatchesName=False",
			filter:  filters.NewArgs(filters.Arg("id", "with")),
		},
		{
			subtest:           "id=with/IDAlsoMatchesName=True",
			filter:            filters.NewArgs(filters.Arg("id", "with")),
			idAlsoMatchesName: true,
			results:           []string{"networkwithcontainer", "networkwithservice"},
		},
		{
			subtest: "id=jbexjvgu/IDAlsoMatchesName=False",
			filter:  filters.NewArgs(filters.Arg("id", "argjbex")),
			results: []string{"networkwithcontainer", "networkwithservice"},
		},
		{
			subtest:           "id=jbexjvgu/IDAlsoMatchesName=True",
			filter:            filters.NewArgs(filters.Arg("id", "argjbex")),
			idAlsoMatchesName: true,
			results:           []string{"networkwithcontainer", "networkwithservice"},
		},
		{
			subtest: "id=my/IDAlsoMatchesName=False",
			filter:  filters.NewArgs(filters.Arg("id", "my")),
			results: []string{"idoverlap"},
		},
		{
			subtest:           "id=my/IDAlsoMatchesName=True",
			filter:            filters.NewArgs(filters.Arg("id", "my")),
			idAlsoMatchesName: true,
			results:           []string{"myoverlay", "mydrivernet", "mykvnet", "idoverlap"},
		},
		{
			subtest:     "Prune/empty",
			filter:      filters.NewArgs(),
			pruneFilter: true,
			// Prune filters have an implicit dangling=true
			results: []string{"myoverlay", "mydrivernet", "mykvnet", "idoverlap"},
		},
		{
			subtest:     "Prune/label=foo",
			filter:      filters.NewArgs(filters.Arg("label", "foo")),
			pruneFilter: true,
			results:     []string{"mydrivernet"},
		},
		{
			subtest:     "Prune/label=foo=bar",
			filter:      filters.NewArgs(filters.Arg("label", "foo=bar")),
			pruneFilter: true,
			results:     []string{"mydrivernet"},
		},
		{
			subtest:     "Prune/label=foo=baz",
			filter:      filters.NewArgs(filters.Arg("label", "foo=baz")),
			pruneFilter: true,
		},
		{
			subtest:     "Prune/label!=foo",
			filter:      filters.NewArgs(filters.Arg("label!", "foo")),
			pruneFilter: true,
			results:     []string{"myoverlay", "mykvnet", "idoverlap"},
		},
		{
			subtest:     "Prune/until=2025-01-01",
			filter:      filters.NewArgs(filters.Arg("until", "2025-01-01")),
			pruneFilter: true,
			results:     []string{"myoverlay", "mydrivernet", "mykvnet"},
		},
		{
			subtest:     "Prune/until=2024-12-01T01:00:00",
			filter:      filters.NewArgs(filters.Arg("until", "2024-12-01T01:00:00")),
			pruneFilter: true,
			results:     []string{"myoverlay"},
		},
		{
			subtest:     "Prune/MultipleTerms=until",
			filter:      filters.NewArgs(filters.Arg("until", "2024-12-01T01:00:00"), filters.Arg("until", "2h")),
			pruneFilter: true,
			err:         "invalid filter: 'until' cannot be specified more than once",
		},
		{
			subtest:     "Prune/id=invalid",
			filter:      filters.NewArgs(filters.Arg("id", "invalid")),
			pruneFilter: true,
			err:         "invalid filter 'id'",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.subtest, func(t *testing.T) {
			var (
				flt Filter
				err error
			)
			if testCase.pruneFilter {
				flt, err = NewPruneFilter(testCase.filter)
			} else {
				flt, err = NewFilter(testCase.filter)
			}
			if testCase.err != "" {
				assert.Check(t, is.ErrorContains(err, testCase.err))
				assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
				return
			}
			assert.NilError(t, err)

			flt.IDAlsoMatchesName = testCase.idAlsoMatchesName
			got := map[string]bool{}
			for _, nw := range networks {
				if flt.Matches(nw) {
					got[nw.Name()] = true
				}
			}

			want := map[string]bool{}
			for _, r := range testCase.results {
				want[r] = true
			}
			assert.Check(t, is.DeepEqual(got, want))
		})
	}
}
