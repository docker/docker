#!/bin/bash
# vim: set noexpandtab:
# -*- indent-tabs-mode: t -*-
set -eu

# Usage: generate-swagger-api.sh [api-directory]
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
API_DIR="${1:-${SCRIPT_DIR}/..}"

"${SCRIPT_DIR}/generate-swagger-spec.sh" "${API_DIR}"

generate_model() {
	local package="$1"
	shift
	mapfile
	swagger generate model --spec="${API_DIR}/swagger.yaml" \
		--target="${API_DIR}" --model-package="$package" \
		--config-file="${API_DIR}/swagger-gen.yaml" \
		--template-dir="${API_DIR}/templates" --allow-template-override \
		"$@" \
		$(printf -- '--name=%s ' "${MAPFILE[@]}")
}

# /==================================================================\
# |                                                                  |
# |  ATTENTION:                                                      |
# |                                                                  |
# |       Sort model package stanzas and model/operation names       |
# |                    *** ALPHABETICALLY ***                        |
# |          to reduce the likelihood of merge conflicts.            |
# |                                                                  |
# \==================================================================/

#region -------- Models --------

generate_model types/build <<- 'EOT'
	BuildCacheDiskUsage
EOT

generate_model types/common <<- 'EOT'
	ErrorResponse
	IDResponse
EOT

generate_model types/container <<- 'EOT'
	ChangeType
	ContainerCreateResponse
	ContainerTopResponse
	ContainerUpdateResponse
	ContainerWaitExitError
	ContainerWaitResponse
	ContainersDiskUsage
	FilesystemChange
	PortSummary
EOT

generate_model types/image <<- 'EOT'
	ImageDeleteResponseItem
	ImagesDiskUsage
	ImageHistoryResponseItem
EOT
#	ImageSummary
# TODO: Restore when go-swagger is updated
# See https://github.com/moby/moby/pull/47526#discussion_r1551800022

generate_model types/network --keep-spec-order --additional-initialism=IPAM <<- 'EOT'
	ConfigReference
	EndpointResource
	IPAMStatus
	Network
	NetworkConnectRequest
	NetworkCreateResponse
	NetworkDisconnectRequest
	NetworkInspect
	NetworkStatus
	NetworkSummary
	NetworkTaskInfo
	PeerInfo
	ServiceInfo
	SubnetStatus
EOT

generate_model types/plugin <<- 'EOT'
	Plugin
	PluginDevice
	PluginEnv
	PluginMount
EOT

generate_model types/registry <<- 'EOT'
	AuthResponse
EOT

generate_model types/storage <<- 'EOT'
	DriverData
	RootFSStorage
	RootFSStorageSnapshot
	Storage
EOT

generate_model types/swarm <<- 'EOT'
	ServiceCreateResponse
	ServiceUpdateResponse
EOT

generate_model types/volume <<- 'EOT'
	Volume
	VolumeCreateRequest
	VolumeListResponse
	VolumesDiskUsage
EOT

#endregion
