package metrics

import (
	"errors"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	gometrics "github.com/docker/go-metrics"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	metricsNS = gometrics.NewNamespace("engine", "daemon", nil)

	// ContainerActions tracks the time taken to process container operations
	ContainerActions = metricsNS.NewLabeledTimer("container_actions", "The number of seconds it takes to process each container action", "action")
	// NetworkActions tracks the time taken to process network operations
	NetworkActions = metricsNS.NewLabeledTimer("network_actions", "The number of seconds it takes to process each network action", "action")
	// HostInfoFunctions tracks the time taken to gather host information
	HostInfoFunctions = metricsNS.NewLabeledTimer("host_info_functions", "The number of seconds it takes to call functions gathering info about the host", "function")
	ImageActions      = metricsNS.NewLabeledTimer("image_actions", "The number of seconds it takes to process each image action", "action")

	// EngineInfo provides information about the engine and its environment
	EngineInfo = metricsNS.NewLabeledGauge("engine", "The information related to the engine and the OS it is running on", gometrics.Unit("info"),
		"version",
		"commit",
		"architecture",
		"graphdriver",
		"kernel",
		"os",
		"os_type",
		"os_version",
		"daemon_id",
	)
	// EngineCPUs tracks the number of CPUs available to the engine
	EngineCPUs = metricsNS.NewGauge("engine_cpus", "The number of cpus that the host system of the engine has", gometrics.Unit("cpus"))
	// EngineMemory tracks the amount of memory available to the engine
	EngineMemory = metricsNS.NewGauge("engine_memory", "The number of bytes of memory that the host system of the engine has", gometrics.Bytes)

	// HealthChecksCounter tracks the total number of health checks
	HealthChecksCounter = metricsNS.NewCounter("health_checks", "The total number of health checks")
	// HealthChecksFailedCounter tracks the number of failed health checks
	HealthChecksFailedCounter = metricsNS.NewCounter("health_checks_failed", "The total number of failed health checks")
	// HealthCheckStartDuration tracks the time taken to prepare health checks
	HealthCheckStartDuration = metricsNS.NewTimer("health_check_start_duration", "The number of seconds it takes to prepare to run health checks")

	// ImageDeletesCounter tracks the total number of image deletion attempts
	ImageDeletesCounter = metricsNS.NewCounter("image_deletes", "The total number of image deletion attempts")
	// ImageDeletesFailedCounter tracks the number of failed image deletions
	ImageDeletesFailedCounter = metricsNS.NewLabeledCounter("image_deletes_failed", "The total number of failed image deletions", "reason")

	// StateCtr tracks container states
	StateCtr = newStateCounter(metricsNS, metricsNS.NewDesc("container_states", "The count of containers in various states", gometrics.Unit("containers"), "state"))

	// EventsCounter tracks the number of events logged
	EventsCounter = metricsNS.NewCounter("events", "The number of events logged")

	// EventSubscribers tracks the number of current subscribers to events
	EventSubscribers = metricsNS.NewGauge("events_subscribers", "The number of current subscribers to events", gometrics.Total)
)

func init() {
	for _, a := range []string{
		"start",
		"changes",
		"commit",
		"create",
		"delete",
	} {
		ContainerActions.WithValues(a).Update(0)
	}

	gometrics.Register(metricsNS)
}

func StartTimer(t gometrics.Timer) func() {
	return gometrics.StartTimer(t)
}

// StateCounter tracks container states
type StateCounter struct {
	mu     sync.RWMutex
	states map[string]string
	desc   *prometheus.Desc
}

func newStateCounter(ns *gometrics.Namespace, desc *prometheus.Desc) *StateCounter {
	c := &StateCounter{
		states: make(map[string]string),
		desc:   desc,
	}
	ns.Add(c)
	return c
}

// Get returns the count of containers in running, paused, and stopped states
func (ctr *StateCounter) Get() (running int, paused int, stopped int) {
	ctr.mu.RLock()
	defer ctr.mu.RUnlock()

	// FIXME(thaJeztah): there's no "container.StateStopped"; should we align these states with actual states?
	for _, state := range ctr.states {
		switch state {
		case "running":
			running++
		case "paused":
			paused++
		case "stopped":
			stopped++
		}
	}
	return running, paused, stopped
}

// Set updates the state for a container
func (ctr *StateCounter) Set(id, label string) {
	ctr.mu.Lock()
	defer ctr.mu.Unlock()

	ctr.states[id] = label
}

// Delete removes a container's state
func (ctr *StateCounter) Delete(id string) {
	ctr.mu.Lock()
	defer ctr.mu.Unlock()

	delete(ctr.states, id)
}

// Describe implements prometheus.Collector
func (ctr *StateCounter) Describe(ch chan<- *prometheus.Desc) {
	ch <- ctr.desc
}

// Collect implements prometheus.Collector
func (ctr *StateCounter) Collect(ch chan<- prometheus.Metric) {
	running, paused, stopped := ctr.Get()
	ch <- prometheus.MustNewConstMetric(ctr.desc, prometheus.GaugeValue, float64(running), "running")
	ch <- prometheus.MustNewConstMetric(ctr.desc, prometheus.GaugeValue, float64(paused), "paused")
	ch <- prometheus.MustNewConstMetric(ctr.desc, prometheus.GaugeValue, float64(stopped), "stopped")
}

// CategorizeErrorReason categorizes an error into a reason string for metrics reporting.
// It supports both containerd errdefs and moby errdefs error types.
func CategorizeErrorReason(err error) string {
	if err == nil {
		return "unknown"
	}

	// Check containerd errdefs
	if cerrdefs.IsNotFound(err) {
		return "not_found"
	}
	if cerrdefs.IsConflict(err) {
		return "conflict"
	}
	if cerrdefs.IsUnauthorized(err) || cerrdefs.IsPermissionDenied(err) {
		return "permission_denied"
	}
	if cerrdefs.IsInvalidArgument(err) {
		return "invalid_argument"
	}

	// Check moby errdefs using interface checks
	var notFound interface{ NotFound() }
	if errors.As(err, &notFound) {
		return "not_found"
	}
	var conflict interface{ Conflict() }
	if errors.As(err, &conflict) {
		return "conflict"
	}
	var unauthorized interface{ Unauthorized() }
	var forbidden interface{ Forbidden() }
	if errors.As(err, &unauthorized) || errors.As(err, &forbidden) {
		return "permission_denied"
	}
	var invalidParam interface{ InvalidParameter() }
	if errors.As(err, &invalidParam) {
		return "invalid_argument"
	}

	return "unknown"
}
