package container

import "github.com/moby/moby/api/types/common"

// ExecCreateResponse is the response for a successful exec-create request.
// It holds the ID of the exec that was created.
//
// TODO(thaJeztah): make this a distinct type.
type ExecCreateResponse = common.IDResponse

// ExecInspectResponse is the API response for the "GET /exec/{id}/json"
// endpoint and holds information about and exec.
type ExecInspectResponse struct {
	ID            string `json:"ID"`
	Running       bool   `json:"Running"`
	ExitCode      *int   `json:"ExitCode"`
	ProcessConfig *ExecProcessConfig
	OpenStdin     bool   `json:"OpenStdin"`
	OpenStderr    bool   `json:"OpenStderr"`
	OpenStdout    bool   `json:"OpenStdout"`
	CanRemove     bool   `json:"CanRemove"`
	ContainerID   string `json:"ContainerID"`
	DetachKeys    []byte `json:"DetachKeys"`
	Pid           int    `json:"Pid"`

	// Labels holds the user-defined metadata declared at exec create time.
	Labels map[string]string `json:"Labels,omitempty"`
}

// ExecProcessConfig holds information about the exec process
// running on the host.
type ExecProcessConfig struct {
	Tty        bool     `json:"tty"`
	Entrypoint string   `json:"entrypoint"`
	Arguments  []string `json:"arguments"`
	Privileged *bool    `json:"privileged,omitempty"`
	User       string   `json:"user,omitempty"`
}
