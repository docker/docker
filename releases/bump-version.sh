#!/usr/bin/env bash
set -Eeuo pipefail

if [ "$#" -ne 3 ]; then
	printf 'usage: %s <component> <last|next|next-minor> <released-version>\n' "$0" >&2
	exit 64
fi

component="$1"
version_operation="$2"
released_version="$3"

versions_file="releases/versions.yaml"
version_pattern='^([0-9]+)\.([0-9]+)\.([0-9]+)$'

version_is_greater() {
	local left_major left_minor left_patch
	local right_major right_minor right_patch

	IFS=. read -r left_major left_minor left_patch <<< "$1"
	IFS=. read -r right_major right_minor right_patch <<< "$2"

	if ((10#$left_major != 10#$right_major)); then
		((10#$left_major > 10#$right_major))
		return
	fi
	if ((10#$left_minor != 10#$right_minor)); then
		((10#$left_minor > 10#$right_minor))
		return
	fi
	((10#$left_patch > 10#$right_patch))
}

read_version() {
	local field="$1"
	local version

	if ! version=$(yq -er ".${component}.${field}" "$versions_file"); then
		printf 'could not find %s.%s in %s\n' \
			"$component" "$field" "$versions_file" >&2
		return 1
	fi
	if [[ ! "$version" =~ $version_pattern ]]; then
		printf 'invalid %s.%s version %q in %s\n' \
			"$component" "$field" "$version" "$versions_file" >&2
		return 1
	fi
	printf '%s\n' "$version"
}

if [[ ! "$released_version" =~ $version_pattern ]]; then
	printf 'invalid released version %q\n' "$released_version" >&2
	exit 64
fi

major=${BASH_REMATCH[1]}
minor=${BASH_REMATCH[2]}
patch=${BASH_REMATCH[3]}

case "$version_operation" in
	last)
		version_field=last
		target_version="$released_version"
		;;
	next)
		version_field=next
		target_version="$major.$minor.$((10#$patch + 1))"
		;;
	next-minor)
		version_field=next
		target_version="$major.$((10#$minor + 1)).0"
		;;
	*)
		printf 'invalid version operation %q, expected last, next, or next-minor\n' \
			"$version_operation" >&2
		exit 64
		;;
esac

last_version=$(read_version last)
next_version=$(read_version next)

if ! version_is_greater "$next_version" "$last_version"; then
	printf 'invalid %s versions in %s: next %s must be newer than last %s\n' \
		"$component" "$versions_file" "$next_version" "$last_version" >&2
	exit 1
fi

case "$version_field" in
	last)
		current_version="$last_version"
		;;
	next)
		current_version="$next_version"
		;;
esac

if ! version_is_greater "$target_version" "$current_version"; then
	printf 'Leaving %s.%s at %s because target version %s is not newer\n' \
		"$component" "$version_field" "$current_version" "$target_version"
	exit 0
fi

if [[ "$version_field" == last ]] \
	&& ! version_is_greater "$next_version" "$target_version"; then
	printf 'Leaving %s.last at %s because target version %s is not older than next %s\n' \
		"$component" "$last_version" "$target_version" "$next_version"
	exit 0
fi

if ! COMPONENT="$component" VERSION_FIELD="$version_field" TARGET_VERSION="$target_version" \
	yq eval -i \
	'.[env(COMPONENT)][env(VERSION_FIELD)] = strenv(TARGET_VERSION)' \
	"$versions_file"; then
	printf 'failed to update %s.%s in %s\n' "$component" "$version_field" "$versions_file" >&2
	exit 1
fi

printf 'Bumped %s.%s from %s to %s\n' \
	"$component" "$version_field" "$current_version" "$target_version"
