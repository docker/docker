package scheduler

import (
	"sort"

	"github.com/docker/swarmkit/api"
)

var (
	defaultFilters = []Filter{
		// Always check for readiness first.
		&ReadyFilter{},
		&ResourceFilter{},
		&PluginFilter{},
		&ConstraintFilter{},
		&PlatformFilter{},
		&HostPortFilter{},
		&MaxReplicasFilter{},
	}
)

type checklistEntry struct {
	f       Filter
	enabled bool

	// failureCount counts the number of nodes that this filter failed
	// against.
	failureCount int
}

type checklistByFailures []checklistEntry

func (c checklistByFailures) Len() int           { return len(c) }
func (c checklistByFailures) Swap(i, j int)      { c[i], c[j] = c[j], c[i] }
func (c checklistByFailures) Less(i, j int) bool { return c[i].failureCount < c[j].failureCount }

// Pipeline runs a set of filters against nodes.
type Pipeline struct {
	// checklist is a slice of filters to run
	checklist []checklistEntry
}

// NewPipeline returns a pipeline with the default set of filters.
func NewPipeline() *Pipeline {
	p := &Pipeline{}

	for _, f := range defaultFilters {
		p.checklist = append(p.checklist, checklistEntry{f: f})
	}

	return p
}

// Process a node through the filter pipeline.
// Returns true if all filters pass, false otherwise.
func (p *Pipeline) Process(n *NodeInfo) bool {
	for i, entry := range p.checklist {
		if entry.enabled && !entry.f.Check(n) {
			// Immediately stop on first failure.
			p.checklist[i].failureCount++
			return false
		}
	}
	for i := range p.checklist {
		p.checklist[i].failureCount = 0
	}
	return true
}

func (p *Pipeline) AddFilter(f Filter) {
	p.checklist = append(p.checklist, checklistEntry{f: f})
}

// SetTask sets up the filters to process a new task. Once this is called,
// Process can be called repeatedly to try to assign the task various nodes.
func (p *Pipeline) SetTask(t *api.Task) {
	for i := range p.checklist {
		p.checklist[i].enabled = p.checklist[i].f.SetTask(t)
		p.checklist[i].failureCount = 0
	}
}

// Explain returns a string explaining why a task could not be scheduled.
func (p *Pipeline) Explain() string {
	var explanation string

	// Sort from most failures to least

	sortedByFailures := make([]checklistEntry, len(p.checklist))
	copy(sortedByFailures, p.checklist)
	sort.Sort(sort.Reverse(checklistByFailures(sortedByFailures)))

	for _, entry := range sortedByFailures {
		if entry.failureCount > 0 {
			if len(explanation) > 0 {
				explanation += "; "
			}
			explanation += entry.f.Explain(entry.failureCount)
		}
	}

	return explanation
}
