package daemon // import "github.com/docker/docker/daemon"

import (
	"sync"

	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/docker/pkg/plugins"
	"github.com/docker/go-metrics"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
)

const metricsPluginType = "MetricsCollector"

var (
	containerActions          metrics.LabeledTimer
	networkActions            metrics.LabeledTimer
	engineInfo                metrics.LabeledGauge
	engineCpus                metrics.Gauge
	engineMemory              metrics.Gauge
	healthChecksCounter       metrics.Counter
	healthChecksFailedCounter metrics.Counter

	stateCtr *stateCounter
)

func init() {
	ns := metrics.NewNamespace("engine", "daemon", nil)
	containerActions = ns.NewLabeledTimer("container_actions", "The number of seconds it takes to process each container action", "action")
	for _, a := range []string{
		"start",
		"changes",
		"commit",
		"create",
		"delete",
	} {
		containerActions.WithValues(a).Update(0)
	}

	networkActions = ns.NewLabeledTimer("network_actions", "The number of seconds it takes to process each network action", "action")
	engineInfo = ns.NewLabeledGauge("engine", "The information related to the engine and the OS it is running on", metrics.Unit("info"),
		"version",
		"commit",
		"architecture",
		"graphdriver",
		"kernel", "os",
		"os_type",
		"daemon_id", // ID is a randomly generated unique identifier (e.g. UUID4)
	)
	engineCpus = ns.NewGauge("engine_cpus", "The number of cpus that the host system of the engine has", metrics.Unit("cpus"))
	engineMemory = ns.NewGauge("engine_memory", "The number of bytes of memory that the host system of the engine has", metrics.Bytes)
	healthChecksCounter = ns.NewCounter("health_checks", "The total number of health checks")
	healthChecksFailedCounter = ns.NewCounter("health_checks_failed", "The total number of failed health checks")

	stateCtr = newStateCounter(ns.NewDesc("container_states", "The count of containers in various states", metrics.Unit("containers"), "state"))
	ns.Add(stateCtr)

	metrics.Register(ns)
}

type stateCounter struct {
	mu     sync.Mutex
	states map[string]string
	desc   *prometheus.Desc
}

func newStateCounter(desc *prometheus.Desc) *stateCounter {
	return &stateCounter{
		states: make(map[string]string),
		desc:   desc,
	}
}

func (ctr *stateCounter) get() (running int, paused int, stopped int) {
	ctr.mu.Lock()
	defer ctr.mu.Unlock()

	states := map[string]int{
		"running": 0,
		"paused":  0,
		"stopped": 0,
	}
	for _, state := range ctr.states {
		states[state]++
	}
	return states["running"], states["paused"], states["stopped"]
}

func (ctr *stateCounter) set(id, label string) {
	ctr.mu.Lock()
	ctr.states[id] = label
	ctr.mu.Unlock()
}

func (ctr *stateCounter) del(id string) {
	ctr.mu.Lock()
	delete(ctr.states, id)
	ctr.mu.Unlock()
}

func (ctr *stateCounter) Describe(ch chan<- *prometheus.Desc) {
	ch <- ctr.desc
}

func (ctr *stateCounter) Collect(ch chan<- prometheus.Metric) {
	running, paused, stopped := ctr.get()
	ch <- prometheus.MustNewConstMetric(ctr.desc, prometheus.GaugeValue, float64(running), "running")
	ch <- prometheus.MustNewConstMetric(ctr.desc, prometheus.GaugeValue, float64(paused), "paused")
	ch <- prometheus.MustNewConstMetric(ctr.desc, prometheus.GaugeValue, float64(stopped), "stopped")
}

func (d *Daemon) cleanupMetricsPlugins() {
	ls := d.PluginStore.GetAllManagedPluginsByCap(metricsPluginType)
	var wg sync.WaitGroup
	wg.Add(len(ls))

	for _, plugin := range ls {
		p := plugin
		go func() {
			defer wg.Done()

			adapter, err := makePluginAdapter(p)
			if err != nil {
				logrus.WithError(err).WithField("plugin", p.Name()).Error("Error creating metrics plugin adapter")
				return
			}
			if err := adapter.StopMetrics(); err != nil {
				logrus.WithError(err).WithField("plugin", p.Name()).Error("Error stopping plugin metrics collection")
			}
		}()
	}
	wg.Wait()

	if d.metricsPluginListener != nil {
		d.metricsPluginListener.Close()
	}
}

type metricsPlugin interface {
	StartMetrics() error
	StopMetrics() error
}

func makePluginAdapter(p plugingetter.CompatPlugin) (metricsPlugin, error) {
	if pc, ok := p.(plugingetter.PluginWithV1Client); ok {
		return &metricsPluginAdapter{pc.Client(), p.Name()}, nil
	}

	pa, ok := p.(plugingetter.PluginAddr)
	if !ok {
		return nil, errdefs.System(errors.Errorf("got unknown plugin type %T", p))
	}

	if pa.Protocol() != plugins.ProtocolSchemeHTTPV1 {
		return nil, errors.Errorf("plugin protocol not supported: %s", pa.Protocol())
	}

	addr := pa.Addr()
	client, err := plugins.NewClientWithTimeout(addr.Network()+"://"+addr.String(), nil, pa.Timeout())
	if err != nil {
		return nil, errors.Wrap(err, "error creating metrics plugin client")
	}
	return &metricsPluginAdapter{client, p.Name()}, nil
}

type metricsPluginAdapter struct {
	c    *plugins.Client
	name string
}

func (a *metricsPluginAdapter) StartMetrics() error {
	type metricsPluginResponse struct {
		Err string
	}
	var res metricsPluginResponse
	if err := a.c.Call(metricsPluginType+".StartMetrics", nil, &res); err != nil {
		return errors.Wrap(err, "could not start metrics plugin")
	}
	if res.Err != "" {
		return errors.New(res.Err)
	}
	return nil
}

func (a *metricsPluginAdapter) StopMetrics() error {
	if err := a.c.Call(metricsPluginType+".StopMetrics", nil, nil); err != nil {
		return errors.Wrap(err, "error stopping metrics collector")
	}
	return nil
}
