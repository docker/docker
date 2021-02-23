package convert // import "github.com/docker/docker/daemon/cluster/convert"

import (
	"strings"

	types "github.com/docker/docker/api/types/swarm"
	swarmapi "github.com/docker/swarmkit/api"
	gogotypes "github.com/gogo/protobuf/types"
)

// TaskFromGRPC converts a grpc Task to a Task.
func TaskFromGRPC(t swarmapi.Task) (types.Task, error) {
	containerStatus := t.Status.GetContainer()
	taskSpec, err := taskSpecFromGRPC(t.Spec)
	if err != nil {
		return types.Task{}, err
	}
	task := types.Task{
		ID:          t.ID,
		Annotations: annotationsFromGRPC(t.Annotations),
		ServiceID:   t.ServiceID,
		Slot:        int(t.Slot),
		NodeID:      t.NodeID,
		Spec:        taskSpec,
		Status: types.TaskStatus{
			State:   types.TaskState(strings.ToLower(t.Status.State.String())),
			Message: t.Status.Message,
			Err:     t.Status.Err,
		},
		DesiredState:     types.TaskState(strings.ToLower(t.DesiredState.String())),
		GenericResources: GenericResourcesFromGRPC(t.AssignedGenericResources),
	}

	// Meta
	task.Version.Index = t.Meta.Version.Index
	task.CreatedAt, _ = gogotypes.TimestampFromProto(t.Meta.CreatedAt)
	task.UpdatedAt, _ = gogotypes.TimestampFromProto(t.Meta.UpdatedAt)

	task.Status.Timestamp, _ = gogotypes.TimestampFromProto(t.Status.Timestamp)

	if containerStatus != nil {
		task.Status.ContainerStatus = &types.ContainerStatus{
			ContainerID: containerStatus.ContainerID,
			PID:         int(containerStatus.PID),
			ExitCode:    int(containerStatus.ExitCode),
		}
	}

	// NetworksAttachments
	for _, na := range t.Networks {
		task.NetworksAttachments = append(task.NetworksAttachments, networkAttachmentFromGRPC(na))
	}

	if t.JobIteration != nil {
		task.JobIteration = &types.Version{
			Index: t.JobIteration.Index,
		}
	}

	// appending to a nil slice is valid. if there are no items in t.Volumes,
	// then the task.Volumes will remain nil; otherwise, it will contain
	// converted entries.
	for _, v := range t.Volumes {
		task.Volumes = append(task.Volumes, types.VolumeAttachment{
			ID:     v.ID,
			Source: v.Source,
			Target: v.Target,
		})
	}

	if t.Status.PortStatus == nil {
		return task, nil
	}

	for _, p := range t.Status.PortStatus.Ports {
		task.Status.PortStatus.Ports = append(task.Status.PortStatus.Ports, types.PortConfig{
			Name:          p.Name,
			Protocol:      types.PortConfigProtocol(strings.ToLower(swarmapi.PortConfig_Protocol_name[int32(p.Protocol)])),
			PublishMode:   types.PortConfigPublishMode(strings.ToLower(swarmapi.PortConfig_PublishMode_name[int32(p.PublishMode)])),
			TargetPort:    p.TargetPort,
			PublishedPort: p.PublishedPort,
		})
	}

	return task, nil
}
