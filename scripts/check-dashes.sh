#!/usr/bin/env sh
# Fail the build if any tracked file contains a U+2014 (em dash) or U+2013
# (en dash). Those characters are visually confusable with the ASCII hyphen
# but survive as multi-byte UTF-8, so they routinely leak through copy-paste
# from docs and PR reviews and then break shell one-liners, grep patterns,
# and diff readability. This script is the tripwire: keep the source tree
# ASCII-only on this axis.
#
# The scan is deliberately byte-level: we pass the literal UTF-8 sequences
# to plain `grep -e` (NOT `grep -P`, which is not portable across all
# grep builds) and let it match anywhere in the file. Files unreadable via
# `git ls-files` are ignored - if a path is not tracked it is not in scope.
#
# Exit codes:
#   0  clean
#   1  at least one dash found (offending files/lines are printed)
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT"

# The two forbidden UTF-8 byte sequences. Octal escapes because POSIX
# printf does not require \x hex support (dash's printf, in particular,
# does not honor it), whereas \NNN octal is portable across sh, dash and
# bash. E2 80 94 = U+2014 (em dash); E2 80 93 = U+2013 (en dash).
EM_DASH=$(printf '\342\200\224')
EN_DASH=$(printf '\342\200\223')

# `git ls-files -z` is NUL-delimited so paths with spaces or newlines are
# safe. `xargs -0 grep -aHn -e ... -e ...` prints file:line for every hit;
# `-a` treats every input as text so a binary that happens to include the
# bytes still surfaces rather than being silently skipped.
hits=$(git ls-files -z | xargs -0 grep -aHn -e "$EM_DASH" -e "$EN_DASH" 2>/dev/null || true)

if [ -n "$hits" ]; then
	echo "check-dashes: forbidden em/en dash characters (U+2014 / U+2013) found:" >&2
	echo "$hits" >&2
	exit 1
fi
exit 0
