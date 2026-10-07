#!/bin/sh
# Writes the "What's changed" section of a cortex release's notes.
#
# Usage: GH_TOKEN=... GITHUB_REPOSITORY=owner/repo release_notes.sh <tag>
#
# GitHub's generate-notes API lists the pull requests merged between two tags.
# This script picks the earlier tag and groups that list by PR title prefix,
# which verify-pr-title makes every PR carry. Labels are GitHub's own way to
# group the list, but few PRs here have one.
#
# Prints nothing when no earlier release exists. Needs the tag's full history:
# release-binaries.yaml checks out with fetch-depth: 0.
#
# Run the tests: sh scripts/release_notes_test.sh
set -eu

tag=${1:?usage: release_notes.sh <tag>}
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}

die() { printf 'release_notes.sh: %s\n' "$1" >&2; exit 1; }

# A shallow clone has no earlier tags, which would read as "first release"
# and quietly drop the section. Fail loudly instead.
[ "$(git rev-parse --is-shallow-repository)" = false ] ||
	die 'shallow clone: check out with fetch-depth: 0'
git rev-parse -q --verify "${tag}^{commit}" >/dev/null || die "no such tag: ${tag}"

# A prerelease is compared with the tag just before it, so rc.3 lists what
# landed after rc.2. A stable release is compared with the stable release
# before it, so v0.9.0 lists the whole cycle and not only what followed the
# last rc. Ancestry, not version order: `sort -V` puts v0.9.0-rc.1 after v0.9.0.
case "${tag}" in
*-*) prev=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "${tag}^" 2>/dev/null) || exit 0 ;;
*) prev=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude 'v*-*' "${tag}^" 2>/dev/null) || exit 0 ;;
esac

body=$(gh api "repos/${repo}/releases/generate-notes" \
	-f tag_name="${tag}" -f previous_tag_name="${prev}" --jq .body)

# The API's body is a "## What's Changed" list of
#   * <PR title> by @<author> in <PR URL>
# then an optional "## New Contributors" list and a "**Full Changelog**" link.
# A title keeps its text and loses its prefix ("Fix: ", "build(deps): ") and any
# gitmoji after it; the prefix picks the section instead.
printf '%s\n' "${body}" | awk -v prev="${prev}" '
function add(sec, line) { items[sec] = items[sec] line "\n"; count[sec]++ }
function section(heading, sec) {
	if (count[sec]) printf "### %s\n\n%s\n", heading, items[sec]
}
function folded(summary, sec) {
	if (count[sec]) printf "<details>\n<summary>%s (%d)</summary>\n\n%s\n</details>\n\n", summary, count[sec], items[sec]
}
BEGIN {
	# The prefixes verify-pr-title accepts, lowercased: it matches case-insensitively.
	n = split("build|chore|ci|docs|feat|feature|fix|bug fix|perf|refactor|revert|style|test|" \
		"proposal|breaking change|other|other/misc", k, "|")
	for (i = 1; i <= n; i++) known[k[i]] = 1
}
/^## New Contributors/ { contrib = 1; next }
/^\*\*Full Changelog\*\*/ { full = $0; next }
!/^\* / { next }
contrib { add("contrib", $0); next }
{
	title = substr($0, 3)
	type = ""
	if (match(title, /^[A-Za-z][A-Za-z \/]*(\([^)]*\))?!?:/)) {
		cand = tolower(substr(title, 1, RLENGTH - 1))
		base = cand
		sub(/!$/, "", base)
		sub(/\(.*$/, "", base)
		# Only a prefix the title gate accepts. "Make X: do Y" is a title.
		if (base in known) {
			type = cand
			title = substr(title, RLENGTH + 1)
		}
	}
	sub(/^[[:space:]]+/, "", title)
	while (match(title, /^:[a-z0-9_+-]+:[[:space:]]*/)) title = substr(title, RLENGTH + 1)
	title = toupper(substr(title, 1, 1)) substr(title, 2)

	if (type ~ /!$/ || type == "breaking change") sec = "breaking"
	else if (type ~ /\(deps\)$/ || $0 ~ / by @dependabot\[bot\] in /) sec = "deps"
	else {
		sub(/\(.*$/, "", type)
		if (type == "feat" || type == "feature") sec = "feat"
		else if (type == "fix" || type == "bug fix" || type == "perf") sec = "fix"
		else sec = "other"
	}
	add(sec, "* " title)
}
END {
	printf "## What\047s changed since %s\n\n", prev
	if (!count["breaking"] && !count["feat"] && !count["fix"] && !count["other"] && !count["deps"])
		printf "No pull requests were merged since %s.\n\n", prev
	section("Breaking changes", "breaking")
	section("New features", "feat")
	section("Bug fixes", "fix")
	folded("Other changes", "other")
	folded("Dependency updates", "deps")
	section("New contributors", "contrib")
	if (full != "") printf "%s\n", full
}'
