package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"time"
)

// MetricsSummary holds a roll-up of metrics info for a given interval
type MetricsSummary struct {
	Timestamp string
	Gauges    []GaugeValue
	Points    []PointValue
	Counters  []SampledValue
	Samples   []SampledValue
}

type GaugeValue struct {
	Name  string
	Hash  string `json:"-"`
	Value float32

	Labels        []Label           `json:"-"`
	DisplayLabels map[string]string `json:"Labels"`
}

type PointValue struct {
	Name   string
	Points []float32
}

type SampledValue struct {
	Name string
	Hash string `json:"-"`
	*AggregateSample
	Mean   float64
	Stddev float64

	Labels        []Label           `json:"-"`
	DisplayLabels map[string]string `json:"Labels"`
}

// DisplayMetrics returns a summary of the metrics from the most recent finished interval.
func (i *InmemSink) DisplayMetrics(resp http.ResponseWriter, req *http.Request) (interface{}, error) {
	data := i.Data()

	var interval *IntervalMetrics
	n := len(data)
	switch {
	case n == 0:
		return nil, fmt.Errorf("no metric intervals have been initialized yet")
	case n == 1:
		// Show the current interval if it's all we have
		interval = i.intervals[0]
	default:
		// Show the most recent finished interval if we have one
		interval = i.intervals[n-2]
	}

	summary := MetricsSummary{
		Timestamp: interval.Interval.Round(time.Second).UTC().String(),
		Gauges:    make([]GaugeValue, 0, len(interval.Gauges)),
		Points:    make([]PointValue, 0, len(interval.Points)),
	}

	// Format and sort the output of each metric type, so it gets displayed in a
	// deterministic order.
	for name, points := range interval.Points {
		summary.Points = append(summary.Points, PointValue{name, points})
	}
	sort.Slice(summary.Points, func(i, j int) bool {
		return summary.Points[i].Name < summary.Points[j].Name
	})

	for hash, value := range interval.Gauges {
		value.Hash = hash
		value.DisplayLabels = make(map[string]string)
		for _, label := range value.Labels {
			value.DisplayLabels[label.Name] = label.Value
		}
		value.Labels = nil

		summary.Gauges = append(summary.Gauges, value)
	}
	sort.Slice(summary.Gauges, func(i, j int) bool {
		return summary.Gauges[i].Hash < summary.Gauges[j].Hash
	})

	summary.Counters = formatSamples(interval.Counters)
	summary.Samples = formatSamples(interval.Samples)

	return summary, nil
}

func formatSamples(source map[string]SampledValue) []SampledValue {
	output := make([]SampledValue, 0, len(source))
	for hash, sample := range source {
		displayLabels := make(map[string]string)
		for _, label := range sample.Labels {
			displayLabels[label.Name] = label.Value
		}

		output = append(output, SampledValue{
			Name:            sample.Name,
			Hash:            hash,
			AggregateSample: sample.AggregateSample,
			Mean:            sample.AggregateSample.Mean(),
			Stddev:          sample.AggregateSample.Stddev(),
			DisplayLabels:   displayLabels,
		})
	}
	sort.Slice(output, func(i, j int) bool {
		return output[i].Hash < output[j].Hash
	})

	return output
}
