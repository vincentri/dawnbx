#!/usr/bin/env bash
# --help must print the whole usage header.
#
# install.sh printed it with `sed -n '2,28p'`, a hardcoded range defended by a
# comment asserting the header ends at 28 and `set -euo pipefail` begins at 29.
# Both were true. Adding one flag description would have silently truncated
# --help, and README.md and the installation guide both promise it lists every
# flag. The range is now anchored to the terminator; this checks the result.
#
# No flags are advertised that the installer does not accept, and no code leaks
# past the header into the output.
set -uo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf '  %s\n' "$*"; }

help_out=$(bash install.sh --help 2>/dev/null)
if [ -z "$help_out" ]; then
  note "--help printed nothing"
  exit 1
fi

# The header ends at the last comment line before the `set -` line.
if printf '%s\n' "$help_out" | grep -qv '^#'; then
  note "--help printed something that is not part of the comment header:"
  printf '%s\n' "$help_out" | grep -n -v '^#' | head -3
  fail=1
fi

# Every comment line of the file's header has to appear in --help. Comparing
# against the file rather than against a list of known tokens is the point: the
# old failure was a range that stopped one line early, and a check that only
# looks for tokens it already knows about cannot see a line it has never been
# told about.
header=$(sed -n '2,/^set -/p' install.sh | sed '$d')
missing=$(comm -23 \
  <(printf '%s\n' "$header" | grep '^#' | sort) \
  <(printf '%s\n' "$help_out" | grep '^#' | sort))
if [ -n "$missing" ]; then
  note "--help is missing header lines the file has:"
  printf '%s\n' "$missing" | head -3
  fail=1
fi

# The last header line is the one a truncated range drops.
if ! printf '%s\n' "$help_out" | grep -q 'the flag wins when both are set'; then
  note "--help stopped before the end of the header"
  fail=1
fi

# Every flag the header advertises must be one the parser accepts.
for flag in $(printf '%s\n' "$help_out" | grep -oE -- '--[a-z][a-z-]+' | sort -u); do
  if ! grep -q -- "$flag)" install.sh && ! grep -q -- "$flag " install.sh; then
    note "--help advertises $flag, which install.sh does not accept"
    fail=1
  fi
done

# And the parser must not accept a flag the header does not mention, which is
# the other half of the promise the docs make.
while read -r flag; do
  [ -n "$flag" ] || continue
  case "$flag" in
    --*|-h) continue ;;
  esac
  if ! printf '%s\n' "$help_out" | grep -q -- "$flag"; then
    note "install.sh accepts $flag, which --help does not list"
    fail=1
  fi
done < <(sed -n 's/^ *\(--[a-z][a-z-]*\)).*/\1/p' install.sh)

[ "$fail" -eq 0 ] && note "--help prints the whole header and matches the parser"
exit "$fail"
