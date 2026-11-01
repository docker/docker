#!/usr/bin/env bash
set -eo pipefail

# This script was developed for use in Moby's CI, and as such the use cases and
# usability are (intentionally) limited. You may find this script useful for
# educational purposes, for example, to learn how pulling images works "under
# the hood", and you may be able to use it for other purposes, but it should not
# be considered a "general purpose" tool for pulling images.
#
# The project maintainers accept contributions to this script within its intended
# scope, but may not accept contributions beyond that.
#
# For users who have a similar need but require more flexibility/functionality,
# refer to the discussion on GitHub, which mentions various alternatives that
# are more suitable for other uses: https://github.com/moby/moby/issues/40857

# hello-world                      latest              ef872312fe1b        3 months ago        910 B
# hello-world                      latest              ef872312fe1bbc5e05aae626791a47ee9b032efa8f3bda39cc0be7b56bfe59b9   3 months ago        910 B

# debian                           latest              f6fab3b798be        10 weeks ago        85.1 MB
# debian                           latest              f6fab3b798be3174f45aa1eb731f8182705555f89c9026d8c1ef230cbf8301dd   10 weeks ago        85.1 MB

# check if essential commands are in our PATH
for cmd in curl jq; do
	if ! command -v $cmd &> /dev/null; then
		echo >&2 "error: \"$cmd\" not found!"
		exit 1
	fi
done

usage() {
	echo "usage: $0 dir image[:tag][@digest] ..."
	echo "       $0 /tmp/old-hello-world hello-world:latest@sha256:8be990ef2aeb16dbcb9271ddfe2610fa6658d13f6dfb8bc72074cc1ca36966a7"
	[ -z "$1" ] || exit "$1"
}

dir="$1" # dir for building tar in
shift || usage 1 >&2

if ! [ $# -gt 0 ] && [ "$dir" ]; then
	usage 2 >&2
fi
mkdir -p "$dir"

# hacky workarounds for Bash 3 support (no associative arrays)
images=()
rm -f "$dir"/tags-*.tmp
manifestJsonEntries=()
doNotGenerateManifestJson=
# repositories[busybox]='"latest": "...", "ubuntu-14.04": "..."'

# bash v4 on Windows CI requires CRLF separator... and linux doesn't seem to care either way
newlineIFS=$'\n'
major=$(echo "${BASH_VERSION%%[^0.9]}" | cut -d. -f1)
if [ "$major" -ge 4 ]; then
	newlineIFS=$'\r\n'
fi

registryBase='https://registry-1.docker.io'
authBase='https://auth.docker.io'
authService='registry.docker.io'

# https://github.com/moby/moby/issues/33700
fetch_blob() {
	local token="$1"
	shift
	local image="$1"
	shift
	local digest="$1"
	shift
	local targetFile="$1"
	shift
	local curlArgs=("$@")

	curl -L -S "${curlArgs[@]}" \
		-H "Authorization: Bearer $token" \
		"$registryBase/v2/$image/blobs/$digest" \
		-o "$targetFile" \
		-D-
}

# handle 'application/vnd.docker.distribution.manifest.v2+json' manifest
handle_single_manifest_v2() {
	local manifestJson="$1"
	shift

	local configDigest
	configDigest="$(echo "$manifestJson" | jq --raw-output '.config.digest')"
	local imageId="${configDigest#*:}" # strip off "sha256:"

	local configFile="$imageId.json"
	fetch_blob "$token" "$image" "$configDigest" "$dir/$configFile" -s

	local layersFs
	layersFs="$(echo "$manifestJson" | jq --raw-output --compact-output '.layers[]')"
	local IFS="$newlineIFS"
	local layers
	mapfile -t layers <<< "$layersFs"
	unset IFS

	echo "Downloading '$imageIdentifier' (${#layers[@]} layers)..."
	local layerId=
	local layerFiles=()
	for i in "${!layers[@]}"; do
		local layerMeta="${layers[$i]}"

		local layerMediaType
		layerMediaType="$(echo "$layerMeta" | jq --raw-output '.mediaType')"
		local layerDigest
		layerDigest="$(echo "$layerMeta" | jq --raw-output '.digest')"

		# save the previous layer's ID
		local parentId="$layerId"
		# create a new fake layer ID based on this layer's digest and the previous layer's fake ID
		layerId="$(echo "$parentId"$'\n'"$layerDigest" | shasum | cut -d' ' -f1)"
		# this accounts for the possibility that an image contains the same layer twice (and thus has a duplicate digest value)

		mkdir -p "$dir/$layerId"
		echo '1.0' > "$dir/$layerId/VERSION"

		if [ ! -s "$dir/$layerId/json" ]; then
			local parentJson
			parentJson="$(printf ', parent: "%s"' "$parentId")"
			local addJson
			addJson="$(printf '{ id: "%s"%s }' "$layerId" "${parentId:+$parentJson}")"
			# this starter JSON is taken directly from Docker's own "docker save" output for unimportant layers
			jq "$addJson + ." > "$dir/$layerId/json" <<- 'EOJSON'
				{
					"created": "0001-01-01T00:00:00Z",
					"container_config": {
						"Hostname": "",
						"Domainname": "",
						"User": "",
						"AttachStdin": false,
						"AttachStdout": false,
						"AttachStderr": false,
						"Tty": false,
						"OpenStdin": false,
						"StdinOnce": false,
						"Env": null,
						"Cmd": null,
						"Image": "",
						"Volumes": null,
						"WorkingDir": "",
						"Entrypoint": null,
						"OnBuild": null,
						"Labels": null
					}
				}
			EOJSON
		fi

		case "$layerMediaType" in
			application/vnd.oci.image.layer.v1.tar+gzip | application/vnd.docker.image.rootfs.diff.tar.gzip)
				local layerTar="$layerId/layer.tar"
				layerFiles=("${layerFiles[@]}" "$layerTar")
				# TODO figure out why "-C -" doesn't work here
				# "curl: (33) HTTP server doesn't seem to support byte ranges. Cannot resume."
				# "HTTP/1.1 416 Requested Range Not Satisfiable"
				if [ -f "$dir/$layerTar" ]; then
					# TODO hackpatch for no -C support :'(
					echo "skipping existing ${layerId:0:12}"
					continue
				fi
				local token
				token="$(curl -fsSL "$authBase/token?service=$authService&scope=repository:$image:pull" | jq --raw-output '.token')"
				fetch_blob "$token" "$image" "$layerDigest" "$dir/$layerTar" --progress-bar
				;;

			*)
				echo >&2 "error: unknown layer mediaType ($imageIdentifier, $layerDigest): '$layerMediaType'"
				exit 1
				;;
		esac
	done

	# change "$imageId" to be the ID of the last layer we added (needed for old-style "repositories" file which is created later -- specifically for older Docker daemons)
	imageId="$layerId"

	# munge the top layer image manifest to have the appropriate image configuration for older daemons
	local imageOldConfig
	imageOldConfig="$(jq --raw-output --compact-output '{ id: .id } + if .parent then { parent: .parent } else {} end' "$dir/$imageId/json")"
	jq --raw-output "$imageOldConfig + del(.history, .rootfs)" "$dir/$configFile" > "$dir/$imageId/json"

	local manifestJsonEntry
	manifestJsonEntry="$(
		echo '{}' | jq --raw-output --compact-output '. + {
			Config: "'"$configFile"'",
			RepoTags: ["'"${image#library\/}:$tag"'"],
			Layers: '"$(echo '[]' | jq --raw-output --compact-output ".$(for layerFile in "${layerFiles[@]}"; do echo " + [ \"$layerFile\" ]"; done)")"'
		}'
	)"
	manifestJsonEntries=("${manifestJsonEntries[@]}" "$manifestJsonEntry")
}

get_target_arch() {
	if [ -n "${TARGETARCH:-}" ]; then
		echo "${TARGETARCH}"
		return 0
	fi

	if type go > /dev/null; then
		go env GOARCH
		return 0
	fi

	if type dpkg > /dev/null; then
		debArch="$(dpkg --print-architecture)"
		case "${debArch}" in
			armel | armhf)
				echo "arm"
				return 0
				;;
			*64el)
				echo "${debArch%el}le"
				return 0
				;;
			*)
				echo "${debArch}"
				return 0
				;;
		esac
	fi

	if type uname > /dev/null; then
		uArch="$(uname -m)"
		case "${uArch}" in
			x86_64)
				echo amd64
				return 0
				;;
			arm | armv[0-9]*)
				echo arm
				return 0
				;;
			aarch64)
				echo arm64
				return 0
				;;
			mips*)
				echo >&2 "I see you are running on mips but I don't know how to determine endianness yet, so I cannot select a correct arch to fetch."
				echo >&2 "Consider installing \"go\" on the system which I can use to determine the correct arch or specify it explicitly by setting TARGETARCH"
				exit 1
				;;
			*)
				echo "${uArch}"
				return 0
				;;
		esac

	fi

	# default value
	echo >&2 "Unable to determine CPU arch, falling back to amd64. You can specify a target arch by setting TARGETARCH"
	echo amd64
}

get_target_variant() {
	echo "${TARGETVARIANT:-}"
}

while [ $# -gt 0 ]; do
	imageTag="$1"
	shift
	image="${imageTag%%[:@]*}"
	imageTag="${imageTag#*:}"
	digest="${imageTag##*@}"
	tag="${imageTag%%@*}"

	# add prefix library if passed official image
	if [[ "$image" != *"/"* ]]; then
		image="library/$image"
	fi

	imageFile="${image//\//_}" # "/" can't be in filenames :)

	token="$(curl -fsSL "$authBase/token?service=$authService&scope=repository:$image:pull" | jq --raw-output '.token')"

	manifestJson="$(
		curl -fsSL \
			-H "Authorization: Bearer $token" \
			-H 'Accept: application/vnd.oci.image.manifest.v1+json' \
			-H 'Accept: application/vnd.oci.image.index.v1+json' \
			-H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
			-H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
			-H 'Accept: application/vnd.docker.distribution.manifest.v1+json' \
			"$registryBase/v2/$image/manifests/$digest"
	)"
	if [ "${manifestJson:0:1}" != '{' ]; then
		echo >&2 "error: /v2/$image/manifests/$digest returned something unexpected:"
		echo >&2 "  $manifestJson"
		exit 1
	fi

	imageIdentifier="$image:$tag@$digest"

	schemaVersion="$(echo "$manifestJson" | jq --raw-output '.schemaVersion')"
	case "$schemaVersion" in
		2)
			mediaType="$(echo "$manifestJson" | jq --raw-output '.mediaType')"

			case "$mediaType" in
				application/vnd.oci.image.manifest.v1+json | application/vnd.docker.distribution.manifest.v2+json)
					handle_single_manifest_v2 "$manifestJson"
					;;
				application/vnd.oci.image.index.v1+json | application/vnd.docker.distribution.manifest.list.v2+json)
					layersFs="$(echo "$manifestJson" | jq --raw-output --compact-output '.manifests[]')"
					IFS="$newlineIFS"
					mapfile -t layers <<< "$layersFs"
					unset IFS

					found=""
					targetArch="$(get_target_arch)"
					targetVariant="$(get_target_variant)"
					# parse first level multi-arch manifest
					for i in "${!layers[@]}"; do
						layerMeta="${layers[$i]}"
						maniArch="$(echo "$layerMeta" | jq --raw-output '.platform.architecture')"
						maniVariant="$(echo "$layerMeta" | jq --raw-output '.platform.variant')"
						if [[ "$maniArch" = "${targetArch}" ]] && [[ -z "${targetVariant}" || "$maniVariant" = "${targetVariant}" ]]; then
							digest="$(echo "$layerMeta" | jq --raw-output '.digest')"
							# get second level single manifest
							submanifestJson="$(
								curl -fsSL \
									-H "Authorization: Bearer $token" \
									-H 'Accept: application/vnd.oci.image.manifest.v1+json' \
									-H 'Accept: application/vnd.oci.image.index.v1+json' \
									-H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
									-H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
									-H 'Accept: application/vnd.docker.distribution.manifest.v1+json' \
									"$registryBase/v2/$image/manifests/$digest"
							)"
							handle_single_manifest_v2 "$submanifestJson"
							found="found"
							break
						fi
					done
					if [ -z "$found" ]; then
						echo >&2 "error: manifest for ${targetArch}${targetVariant:+/${targetVariant}} is not found"
						exit 1
					fi
					;;
				*)
					echo >&2 "error: unknown manifest mediaType ($imageIdentifier): '$mediaType'"
					exit 1
					;;
			esac
			;;

		1)
			if [ -z "$doNotGenerateManifestJson" ]; then
				echo >&2 "warning: '$imageIdentifier' uses schemaVersion '$schemaVersion'"
				echo >&2 "  this script cannot (currently) recreate the 'image config' to put in a 'manifest.json' (thus any schemaVersion 2+ images will be imported in the old way, and their 'docker history' will suffer)"
				echo >&2
				doNotGenerateManifestJson=1
			fi

			layersFs="$(echo "$manifestJson" | jq --raw-output '.fsLayers | .[] | .blobSum')"
			IFS="$newlineIFS"
			mapfile -t layers <<< "$layersFs"
			unset IFS

			history="$(echo "$manifestJson" | jq '.history | [.[] | .v1Compatibility]')"
			imageId="$(echo "$history" | jq --raw-output '.[0]' | jq --raw-output '.id')"

			echo "Downloading '$imageIdentifier' (${#layers[@]} layers)..."
			for i in "${!layers[@]}"; do
				imageJson="$(echo "$history" | jq --raw-output ".[${i}]")"
				layerId="$(echo "$imageJson" | jq --raw-output '.id')"
				imageLayer="${layers[$i]}"

				mkdir -p "$dir/$layerId"
				echo '1.0' > "$dir/$layerId/VERSION"

				echo "$imageJson" > "$dir/$layerId/json"

				# TODO figure out why "-C -" doesn't work here
				# "curl: (33) HTTP server doesn't seem to support byte ranges. Cannot resume."
				# "HTTP/1.1 416 Requested Range Not Satisfiable"
				if [ -f "$dir/$layerId/layer.tar" ]; then
					# TODO hackpatch for no -C support :'(
					echo "skipping existing ${layerId:0:12}"
					continue
				fi
				token="$(curl -fsSL "$authBase/token?service=$authService&scope=repository:$image:pull" | jq --raw-output '.token')"
				fetch_blob "$token" "$image" "$imageLayer" "$dir/$layerId/layer.tar" --progress-bar
			done
			;;

		*)
			echo >&2 "error: unknown manifest schemaVersion ($imageIdentifier): '$schemaVersion'"
			exit 1
			;;
	esac

	echo

	if [ -s "$dir/tags-$imageFile.tmp" ]; then
		echo -n ', ' >> "$dir/tags-$imageFile.tmp"
	else
		images=("${images[@]}" "$image")
	fi
	echo -n '"'"$tag"'": "'"$imageId"'"' >> "$dir/tags-$imageFile.tmp"
done

echo -n '{' > "$dir/repositories"
firstImage=1
for image in "${images[@]}"; do
	imageFile="${image//\//_}" # "/" can't be in filenames :)
	image="${image#library\/}"

	[ "$firstImage" ] || echo -n ',' >> "$dir/repositories"
	firstImage=
	echo -n $'\n\t' >> "$dir/repositories"
	echo -n '"'"$image"'": { '"$(cat "$dir/tags-$imageFile.tmp")"' }' >> "$dir/repositories"
done
echo -n $'\n}\n' >> "$dir/repositories"

rm -f "$dir"/tags-*.tmp

if [ -z "$doNotGenerateManifestJson" ] && [ "${#manifestJsonEntries[@]}" -gt 0 ]; then
  if [ -f $dir/manifest.json ] && [ $(echo $(<$dir/manifest.json) | jq empty; echo $?) = 0 ]; then
    tmpManifestJsonEntries=$(<$dir/manifest.json)
  else
    tmpManifestJsonEntries=[]
  fi
  echo $tmpManifestJsonEntries | jq ".$(for entry in "${manifestJsonEntries[@]}"; do echo " += [ $entry ]"; done)" | jq 'unique' > "$dir/manifest.json"
else
	rm -f "$dir/manifest.json"
fi

echo "Download of images into '$dir' complete."
echo "Use something like the following to load the result into a Docker daemon:"
echo "  tar -cC '$dir' . | docker load"
