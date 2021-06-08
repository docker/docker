package api

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// AutopilotConfiguration is used for querying/setting the Autopilot configuration.
// Autopilot helps manage operator tasks related to Consul servers like removing
// failed servers from the Raft quorum.
type AutopilotConfiguration struct {
	// CleanupDeadServers controls whether to remove dead servers from the Raft
	// peer list when a new server joins
	CleanupDeadServers bool

	// LastContactThreshold is the limit on the amount of time a server can go
	// without leader contact before being considered unhealthy.
	LastContactThreshold *ReadableDuration

	// MaxTrailingLogs is the amount of entries in the Raft Log that a server can
	// be behind before being considered unhealthy.
	MaxTrailingLogs uint64

	// ServerStabilizationTime is the minimum amount of time a server must be
	// in a stable, healthy state before it can be added to the cluster. Only
	// applicable with Raft protocol version 3 or higher.
	ServerStabilizationTime *ReadableDuration

	// (Enterprise-only) RedundancyZoneTag is the node tag to use for separating
	// servers into zones for redundancy. If left blank, this feature will be disabled.
	RedundancyZoneTag string

	// (Enterprise-only) DisableUpgradeMigration will disable Autopilot's upgrade migration
	// strategy of waiting until enough newer-versioned servers have been added to the
	// cluster before promoting them to voters.
	DisableUpgradeMigration bool

	// (Enterprise-only) UpgradeVersionTag is the node tag to use for version info when
	// performing upgrade migrations. If left blank, the Consul version will be used.
	UpgradeVersionTag string

	// CreateIndex holds the index corresponding the creation of this configuration.
	// This is a read-only field.
	CreateIndex uint64

	// ModifyIndex will be set to the index of the last update when retrieving the
	// Autopilot configuration. Resubmitting a configuration with
	// AutopilotCASConfiguration will perform a check-and-set operation which ensures
	// there hasn't been a subsequent update since the configuration was retrieved.
	ModifyIndex uint64
}

// ServerHealth is the health (from the leader's point of view) of a server.
type ServerHealth struct {
	// ID is the raft ID of the server.
	ID string

	// Name is the node name of the server.
	Name string

	// Address is the address of the server.
	Address string

	// The status of the SerfHealth check for the server.
	SerfStatus string

	// Version is the Consul version of the server.
	Version string

	// Leader is whether this server is currently the leader.
	Leader bool

	// LastContact is the time since this node's last contact with the leader.
	LastContact *ReadableDuration

	// LastTerm is the highest leader term this server has a record of in its Raft log.
	LastTerm uint64

	// LastIndex is the last log index this server has a record of in its Raft log.
	LastIndex uint64

	// Healthy is whether or not the server is healthy according to the current
	// Autopilot config.
	Healthy bool

	// Voter is whether this is a voting server.
	Voter bool

	// StableSince is the last time this server's Healthy value changed.
	StableSince time.Time
}

// OperatorHealthReply is a representation of the overall health of the cluster
type OperatorHealthReply struct {
	// Healthy is true if all the servers in the cluster are healthy.
	Healthy bool

	// FailureTolerance is the number of healthy servers that could be lost without
	// an outage occurring.
	FailureTolerance int

	// Servers holds the health of each server.
	Servers []ServerHealth
}

// ReadableDuration is a duration type that is serialized to JSON in human readable format.
type ReadableDuration time.Duration

func NewReadableDuration(dur time.Duration) *ReadableDuration {
	d := ReadableDuration(dur)
	return &d
}

func (d *ReadableDuration) String() string {
	return d.Duration().String()
}

func (d *ReadableDuration) Duration() time.Duration {
	if d == nil {
		return time.Duration(0)
	}
	return time.Duration(*d)
}

func (d *ReadableDuration) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`"%s"`, d.Duration().String())), nil
}

func (d *ReadableDuration) UnmarshalJSON(raw []byte) error {
	if d == nil {
		return fmt.Errorf("cannot unmarshal to nil pointer")
	}

	str := string(raw)
	if len(str) < 2 || str[0] != '"' || str[len(str)-1] != '"' {
		return fmt.Errorf("must be enclosed with quotes: %s", str)
	}
	dur, err := time.ParseDuration(str[1 : len(str)-1])
	if err != nil {
		return err
	}
	*d = ReadableDuration(dur)
	return nil
}

// AutopilotGetConfiguration is used to query the current Autopilot configuration.
func (op *Operator) AutopilotGetConfiguration(q *QueryOptions) (*AutopilotConfiguration, error) {
	r := op.c.newRequest("GET", "/v1/operator/autopilot/configuration")
	r.setQueryOptions(q)
	_, resp, err := requireOK(op.c.doRequest(r))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out AutopilotConfiguration
	if err := decodeBody(resp, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// AutopilotSetConfiguration is used to set the current Autopilot configuration.
func (op *Operator) AutopilotSetConfiguration(conf *AutopilotConfiguration, q *WriteOptions) error {
	r := op.c.newRequest("PUT", "/v1/operator/autopilot/configuration")
	r.setWriteOptions(q)
	r.obj = conf
	_, resp, err := requireOK(op.c.doRequest(r))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// AutopilotCASConfiguration is used to perform a Check-And-Set update on the
// Autopilot configuration. The ModifyIndex value will be respected. Returns
// true on success or false on failures.
func (op *Operator) AutopilotCASConfiguration(conf *AutopilotConfiguration, q *WriteOptions) (bool, error) {
	r := op.c.newRequest("PUT", "/v1/operator/autopilot/configuration")
	r.setWriteOptions(q)
	r.params.Set("cas", strconv.FormatUint(conf.ModifyIndex, 10))
	r.obj = conf
	_, resp, err := requireOK(op.c.doRequest(r))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return false, fmt.Errorf("Failed to read response: %v", err)
	}
	res := strings.Contains(buf.String(), "true")

	return res, nil
}

// AutopilotServerHealth
func (op *Operator) AutopilotServerHealth(q *QueryOptions) (*OperatorHealthReply, error) {
	r := op.c.newRequest("GET", "/v1/operator/autopilot/health")
	r.setQueryOptions(q)
	_, resp, err := requireOK(op.c.doRequest(r))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out OperatorHealthReply
	if err := decodeBody(resp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
