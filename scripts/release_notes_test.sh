#!/bin/sh
# Tests for release_notes.sh.
#
# Black-box: each case builds a throwaway git repository with the tags it
# needs and puts a `gh` stub first on PATH. The stub records its arguments and
# prints a fixture in place of the generate-notes API's body. No network.
#
# Run: sh scripts/release_notes_test.sh
set -eu

# shellcheck disable=SC1007 # `CDPATH= cd` empties CDPATH for this one command.
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
NOTES_SH="${SCRIPT_DIR}/release_notes.sh"
[ -f "${NOTES_SH}" ] || { printf 'cannot find %s\n' "${NOTES_SH}" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "${TMP}"' EXIT

PASS=0
FAIL=0

check() { # label expected actual
	if [ "$2" = "$3" ]; then
		PASS=$((PASS + 1))
		printf '  ok   %s\n' "$1"
	else
		FAIL=$((FAIL + 1))
		printf '  FAIL %s\n       expected %s\n       actual   %s\n' "$1" "$2" "$3"
	fi
}

check_fails() { # label status
	if [ "$2" != "0" ]; then
		PASS=$((PASS + 1))
		printf '  ok   %s (exit %s)\n' "$1" "$2"
	else
		FAIL=$((FAIL + 1))
		printf '  FAIL %s: expected non-zero exit, got 0\n' "$1"
	fi
}

# The developer's own git config (signing, hooks, default branch) stays out.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

mkdir -p "${TMP}/bin"
cat >"${TMP}/bin/gh" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"${GH_LOG}"
[ -z "${GH_FAIL:-}" ] || exit 1
cat "${GH_BODY}"
EOF
chmod +x "${TMP}/bin/gh"
export GH_LOG="${TMP}/gh.log" GH_BODY="${TMP}/body.md"

# repo <dir> <tag-or-dash>...: one empty commit per argument, tagged unless "-".
repo() {
	dir="${TMP}/$1"
	shift
	git init -q "${dir}"
	for t in "$@"; do
		git -C "${dir}" commit -q --allow-empty -m "c ${t}"
		[ "${t}" = - ] || git -C "${dir}" tag "${t}"
	done
}

# notes <dir> <tag>: run the script there. Output in $OUT, exit status in $STATUS.
OUT="${TMP}/out.md"
notes() {
	: >"${GH_LOG}"
	STATUS=0
	(cd "${TMP}/$1" && GITHUB_REPOSITORY=o/r PATH="${TMP}/bin:${PATH}" sh "${NOTES_SH}" "$2") \
		>"${OUT}" 2>"${TMP}/err" || STATUS=$?
}

# The previous_tag_name the script asked the API for.
asked_prev() { sed -n 's/.*previous_tag_name=\([^ ]*\).*/\1/p' "${GH_LOG}"; }

# The heading of the section holding the bullet that contains $1.
section_of() {
	awk -v s="$1" '/^### |^<summary>/ { h = $0 } /^\* / && index($0, s) { print h; exit }' "${OUT}"
}

printf 'Which release a release is compared against\n'

: >"${GH_BODY}"
repo hist v0.8.0 v0.8.1 v0.9.0-rc.1 v0.9.0-rc.2 main-latest v0.9.0-rc.3
notes hist v0.9.0-rc.3
check "a prerelease is compared with the tag before it, skipping a non-v tag" \
	v0.9.0-rc.2 "$(asked_prev)"
check "the heading names it" \
	"## What's changed since v0.9.0-rc.2" "$(sed -n 1p "${OUT}")"

git -C "${TMP}/hist" tag v0.9.0 v0.9.0-rc.3
notes hist v0.9.0
check "a stable release is compared with the stable release before it" \
	v0.8.1 "$(asked_prev)"

repo first v0.1.0
notes first v0.1.0
check "the first release prints nothing" "0 0 0" \
	"${STATUS} $(wc -c <"${OUT}" | tr -d ' ') $(wc -l <"${GH_LOG}" | tr -d ' ')"

repo untagged - - v0.1.0
notes untagged v0.1.0
check "a release with no earlier v tag prints nothing" "0 0" \
	"${STATUS} $(wc -c <"${OUT}" | tr -d ' ')"

notes hist v9.9.9
check_fails "an unknown tag fails" "${STATUS}"

git clone -q --depth 1 "file://${TMP}/hist" "${TMP}/shallow" 2>/dev/null
git -C "${TMP}/shallow" fetch -q --depth 1 origin tag v0.9.0-rc.3 2>/dev/null
notes shallow v0.9.0-rc.3
check_fails "a shallow clone fails rather than reading as a first release" "${STATUS}"
check "and says why" yes "$(grep -q 'fetch-depth: 0' "${TMP}/err" && echo yes)"

# Exported and unset by hand: whether `VAR=x func` reaches func's children
# differs between shells.
export GH_FAIL=1
notes hist v0.9.0-rc.3
unset GH_FAIL
check_fails "a failed API call fails the script" "${STATUS}"

printf '\nGrouping by title prefix\n'

base=https://github.com/o/r/pull
cat >"${GH_BODY}" <<EOF
## What's Changed
* Feat: Add the archive by @a in ${base}/1
* feature: add a pane by @a in ${base}/2
* Fix: Stop a crash by @b in ${base}/3
* fix: :bug: fix the chart caption by @b in ${base}/4
* Bug fix: Close a leak by @b in ${base}/5
* Perf: Halve startup by @b in ${base}/6
* feat(api)!: Rename the endpoint by @c in ${base}/7
* Breaking change: Drop the old flag by @c in ${base}/8
* Docs: Rewrite the quickstart by @d in ${base}/9
* Test: Cover the parser by @d in ${base}/10
* Make X: work without Y by @d in ${base}/11
* build(deps): Bump foo from 1 to 2 by @dependabot[bot] in ${base}/12
* chore(deps): Bump bar from 3 to 4 by @e in ${base}/13
* Update baz requirement by @dependabot[bot] in ${base}/14

## New Contributors
* @c made their first contribution in ${base}/7

**Full Changelog**: https://github.com/o/r/compare/v0.9.0-rc.2...v0.9.0-rc.3
EOF
notes hist v0.9.0-rc.3

check "Feat goes under New features" "### New features" "$(section_of 'Add the archive')"
check "the prefix matches case-insensitively" "### New features" "$(section_of 'Add a pane')"
check "Fix goes under Bug fixes" "### Bug fixes" "$(section_of 'Stop a crash')"
check "Bug fix goes under Bug fixes" "### Bug fixes" "$(section_of 'Close a leak')"
check "Perf goes under Bug fixes" "### Bug fixes" "$(section_of 'Halve startup')"
check "a ! after the type is a breaking change" "### Breaking changes" "$(section_of 'Rename the endpoint')"
check "Breaking change goes under Breaking changes" "### Breaking changes" "$(section_of 'Drop the old flag')"
check "Docs goes under Other changes" "<summary>Other changes (3)</summary>" "$(section_of 'Rewrite the quickstart')"
check "a colon that is not a known prefix leaves the title whole" \
	"* Make X: work without Y by @d in ${base}/11" "$(grep -F 'work without Y' "${OUT}")"
check "a (deps) scope goes under Dependency updates" \
	"<summary>Dependency updates (3)</summary>" "$(section_of 'Bump bar')"
check "a Dependabot PR with no prefix goes under Dependency updates" \
	"<summary>Dependency updates (3)</summary>" "$(section_of 'Update baz requirement')"
check "the prefix and a gitmoji are dropped and the title capitalised" \
	"* Fix the chart caption by @b in ${base}/4" "$(grep -F 'chart caption' "${OUT}")"
check "sections come in order and empty ones are left out" \
	"## What's changed since v0.9.0-rc.2|### Breaking changes|### New features|### Bug fixes|<summary>Other changes (3)</summary>|<summary>Dependency updates (3)</summary>|### New contributors" \
	"$(grep -E '^(## |### |<summary>)' "${OUT}" | paste -sd '|' -)"
check "new contributors are kept" \
	"* @c made their first contribution in ${base}/7" "$(grep -F 'first contribution' "${OUT}")"
check "the full changelog link is last" \
	"**Full Changelog**: https://github.com/o/r/compare/v0.9.0-rc.2...v0.9.0-rc.3" "$(tail -n 1 "${OUT}")"

printf '## What%sChanged\n\n\n**Full Changelog**: https://github.com/o/r/compare/a...b\n' "'s " >"${GH_BODY}"
notes hist v0.9.0-rc.3
check "an empty list says so" "No pull requests were merged since v0.9.0-rc.2." "$(sed -n 3p "${OUT}")"

printf '\n%s passed, %s failed\n' "${PASS}" "${FAIL}"
[ "${FAIL}" = "0" ]
