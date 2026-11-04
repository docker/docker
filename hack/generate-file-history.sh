#!/usr/bin/env bash
set -euo pipefail

# This script produces a TSV containing the dates and commits at which files
# were first introduced. It can be used to add copyright headers.
#
# Some relevant dates / commits:
#
# - Docker Open Sourced as Apache 2 (Feb 18, 2013): https://github.com/moby/moby/commit/a7e9582a53663453d0885b1a0217941ad1fe595f
# - dotCloud, Inc. -> Docker, Inc. (Oct 29, 2013): https://web.archive.org/web/20210515031439/https://www.businesswire.com/news/home/20131029005746/en/dotCloud-Inc.-is-Now-Docker-Inc.
# - dotCloud, Inc. -> Docker, Inc. (March 13, 2014): https://github.com/moby/moby/commit/73596b00e053fedbf42f7abb87728e7176e5a95c

command -v parallel > /dev/null 2>&1 || {
	echo "parallel is not installed"
	exit 2
}

# all files, excluding "vendor" (which can be nested, e.g. 'man/vendor') and bundles
git ls-files -- ':(exclude)vendor/**' ':(exclude)**/vendor/**' ':(exclude)bundles/**' > files.txt

out="file-history.tsv"
[ -s "$out" ] || printf "filename\tgenerated\tvendored_history\tfirst_path\tfirst_date\tfirst_commit\tauthor\n" > "$out"

# create todo-list, removing already processed paths (first column)
tmpdone="$(mktemp)"
trap 'rm -f "$tmpdone" "$worker" todo.txt' EXIT
tail -n +2 "$out" 2> /dev/null | cut -f1 | LC_ALL=C sort > "$tmpdone" || true
LC_ALL=C sort files.txt > all-files.txt
comm -23 all-files.txt "$tmpdone" > todo.txt
rm -f all-files.txt

worker="$(mktemp)"
cat > "$worker" <<- 'WORKER'
	#!/usr/bin/env bash
	set -euo pipefail
	f="$1"

	# detect generated files
	generated=no
	case "$f" in
		*.golden|*.pb.go|*.mod|*.sum|*.pem)
			generated=yes
			;;
		*.gotmpl)
			# template itself is not generated.
			;;
		daemon/builder/remotecontext/internal/tarsum/testdata/*)
			generated=yes
			;;
		*)
			if head -n 10 -- "$f" 2>/dev/null | grep -qi "DO NOT EDIT"; then
				generated=yes
			fi
			;;
	esac

	# get the first commit and date introducing this file (follow renames) as well
	# as the original filename and author.
	first_commit=$(git -c diff.renameLimit=100000 log --follow --find-renames=40% --diff-filter=A --format=%H -- "$f" | tail -n1)
	[ -z "$first_commit" ] && exit 0

	first_date=$(git show -s --date=short --format=%ad "$first_commit")
	first_path=$(git -c diff.renameLimit=100000 log -1 --follow --find-renames=40% --diff-filter=A --name-only --pretty=format: -- "$f")
	# first_path=$(git show --pretty=format: --name-only "$first_commit" -- "$f" | head -n1)
	first_author=$(git show -s --format="%aN <%aE>" --use-mailmap "$first_commit")

	# Mark files for which the original path was "vendor/" or "Godeps".
	# These files may have their original history elsewhere or further down
	# git history. Some files were moved to a separate repository, but later
	# moved back.
	vendored_history=no
	case "$first_path" in
		vendor/*|*/vendor/*|Godeps/_workspace/*|*/Godeps/_workspace/*)
			vendored_history=yes
			;;
	esac

	printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\n" "$f" "$generated" "$vendored_history" "$first_path" "$first_date" "$first_commit" "$first_author"
WORKER

chmod +x "$worker"
parallel --jobs 8 --no-run-if-empty --bar "$worker" :::: todo.txt >> "$out"
