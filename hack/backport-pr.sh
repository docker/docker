#!/usr/bin/env bash

set -u -o pipefail

log_error() {
	if [ -n "${GITHUB_ACTIONS:-}" ]; then
		echo "::error::${@}"
		return
	fi
	echo "$@" >&2
}

PR="${1}"

# Some round about stuff to run either locally or in github actions
: "${GITHUB_SERVER_URL:=https://github.com}"
if [ -v 2 ]; then
	GITHUB_REPO="${2}"
fi
: "${GITHUB_REPO:=${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}}"

content="$(gh pr view -R "${GITHUB_REPO}" --json title --json labels --json commits "${PR}")"
if [ $? -ne 0 ]; then
	log_error "Error getting PR #${PR} to backport"
	exit 1
fi

labels="$(jq -r '.labels[].name' <<<"${content}" | grep cherry-pick/)"
commits="$(jq -r '.commits[].oid' 2>&1 <<<"${content}")"
title="$(jq -r '.title' <<<"${content}")"

if [ $? -ne 0 ] || [ -z "${commits}" ]; then
	log_error "Error getting commits for PR #${PR} to backport: ${commits}"
	exit 1
fi

dir="$(mktemp -d)"
remote="cherry_pick_origin_for_${PR}"
cleanup() {
	rm -rf "${dir}"
	git remote remove "${remote}"
}

trap cleanup EXIT

git remote add "${remote}" "${GITHUB_REPO}.git"

backport() {
	(
		branch="${1}"

		new_branch="cherry_pick_pr${PR}_to_${branch}"
		trap "cd -; git worktree remove -f ${dir}; git branch -D ${new_branch}" EXIT
		git fetch "${remote}" $branch:$branch >/dev/null || return 2
		git worktree add -b "${new_branch}" "${dir}" $branch >/dev/null || return 3

		cd "${dir}"
		for i in ${commits}; do
			git cherry-pick -x -s $i >/dev/null || return 3
		done

		git push -f "${remote}" "${new_branch}" >/dev/null || return 4
		gh pr create -R "${GITHUB_REPO}" --base "${branch}" --head "${new_branch}" \
			--title "[${branch}] ${title}" \
			--body "Backport PR #${PR} to $branch"
	)
}

for label in $labels; do
	branch="${label#process/cherry-pick/}"
	out="$(backport "${branch}" 2>&1)"
	if [ $? -eq 0 ]; then
		gh pr comment -R "${GITHUB_REPO}" "${PR}" -b "Backport this change to $branch: ${out}"
		continue
	fi
	log_error "Error cherry-picking PR #${PR} to $branch: ${out}"
	gh pr comment -R "${GITHUB_REPO}" "${PR}" -b "Failed to backport this change to $branch: ${out}"
	continue
done
