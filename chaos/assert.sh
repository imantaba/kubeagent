# Machine-checked assertions for the chaos harness.
#
# Sourced by chaos/run.sh and by chaos/assert-selftest.sh (which exercises these
# helpers with no cluster). Each expect_* helper prints one PASS/FAIL line on
# stdout — so a call inside a `{ ... } | record ...` block lands in the report
# beside the value it checked — and appends the same outcome to $ASSERTLOG.
#
# $ASSERTLOG is a FILE, not a shell variable, and that is load-bearing: record()
# is fed by a pipeline, a pipeline runs in a subshell, and a counter incremented
# there would be discarded the moment the block ended. A file survives.
#
# No helper ever returns non-zero. The harness runs under `set -e`; a failing
# assertion must let the remaining scenarios run and surface at the end, in the
# exit code, rather than aborting the suite half way through.
#
# Assertions are written at kubeagent's contract level, never Kubernetes'. Assert
# on what kubeagent did with an API message — a finding is present, a counter, an
# exit code — never on the API server's wording, which moves between minors and
# would go red on a change that broke nothing.

assert_init() {
  ASSERTLOG="${ASSERTLOG:-$(mktemp)}"
  SKIPLOG="${SKIPLOG:-$(mktemp)}"
  # The corpus's scratch shares this lifecycle deliberately: same mktemp, same
  # single trap line. A second `trap ... EXIT` anywhere would clobber this one.
  CORPUSTMP="${CORPUSTMP:-$(mktemp)}"
  : > "$ASSERTLOG"
  : > "$SKIPLOG"
  : > "$CORPUSTMP"
  trap 'rm -f "${ASSERTLOG:-}" "${SKIPLOG:-}" "${CORPUSTMP:-}"' EXIT
}

# _assert_record <PASS|FAIL> <label> <detail>
_assert_record() {
  printf '%s: %s %s\n' "$1" "$2" "$3"
  printf '%s\t%s %s\n' "$1" "$2" "$3" >> "$ASSERTLOG"
  # A failure also goes to the console, so an operator watching a 40-minute run
  # sees it when it happens instead of at the end.
  if [ "$1" = FAIL ]; then printf 'ASSERTION FAILED: %s %s\n' "$2" "$3" >&2; fi
  return 0
}

# assert_skip <label> <reason> — a scenario that did not run, and why.
#
# A skip is not an assertion and not a failure: it never touches $ASSERTLOG and
# it never moves the exit code. It is a STATED GAP, which is the whole point —
# a run that quietly omitted nine scenarios would otherwise look exactly like a
# full green one.
#
# $SKIPLOG is a file for the same reason $ASSERTLOG is: the caller may be inside
# a pipeline, a pipeline runs in a subshell, and a counter incremented there is
# discarded when the block ends.
#
# Like every helper here it returns 0, so `set -e` cannot turn a recorded
# outcome into an aborted run.
assert_skip() {
  printf 'SKIP: %s — %s\n' "$1" "$2"
  printf 'SKIP\t%s — %s\n' "$1" "$2" >> "$SKIPLOG"
  return 0
}

# scenario_title <function name> — the report heading for a skipped scenario,
# derived from the scenario's own function name: scenario_05_coredns -> "5.
# coredns", scenario_14 -> "14.". Deriving it means a skip heading can never
# drift from the scenario it names, which passing the title in as a second
# argument would eventually allow.
#
# 10# forces base 10 on the number: a leading-zero numeral is octal to $(( )),
# so a bare $((08)) is an error rather than 8 — the same trap run.sh's --only
# normalization already documents.
#
# It lives here, not in run.sh, so chaos/assert-selftest.sh can exercise it with
# no cluster; it touches nothing outside its own arguments.
scenario_title() {
  local rest="${1#scenario_}" num word
  case "$rest" in
    *_*) num="${rest%%_*}"; word="${rest#*_}" ;;
    *)   num="$rest";       word="" ;;
  esac
  printf '%s.%s\n' "$((10#$num))" "${word:+ $word}"
}

# expect_eq <label> <actual> <want> — exact string equality.
expect_eq() {
  if [ "$2" = "$3" ]; then
    _assert_record PASS "$1" "($2)"
  else
    _assert_record FAIL "$1" "(got '$2', want '$3')"
  fi
  return 0
}

# expect_ge <label> <actual> <min> — numeric floor. A non-integer actual FAILS
# rather than erroring: ready_replicas() prints "?" when the query itself failed,
# and "couldn't tell" must never be coerced into a confident number.
expect_ge() {
  case "$2" in
    ''|*[!0-9]*)
      _assert_record FAIL "$1" "(got '$2', want an integer >= $3)"
      return 0 ;;
  esac
  if [ "$2" -ge "$3" ]; then
    _assert_record PASS "$1" "($2 >= $3)"
  else
    _assert_record FAIL "$1" "(got $2, want >= $3)"
  fi
  return 0
}

# expect_contains <label> <haystack> <needle> — fixed-string search. The haystack
# is never echoed into the message: it is usually a whole scan.
expect_contains() {
  if printf '%s\n' "$2" | grep -qF -- "$3"; then
    _assert_record PASS "$1" "(found '$3')"
  else
    _assert_record FAIL "$1" "(missing '$3')"
  fi
  return 0
}

# expect_absent <label> <haystack> <needle> — the inverse of expect_contains.
expect_absent() {
  if printf '%s\n' "$2" | grep -qF -- "$3"; then
    _assert_record FAIL "$1" "(found '$3', want it absent)"
  else
    _assert_record PASS "$1" "(absent '$3')"
  fi
  return 0
}

# assert_summary <report-file> — append the tally to the report, print it to the
# console, and return non-zero if any assertion failed. That return status is
# what makes ./chaos/run.sh a gate rather than a report.
#
# Skipped scenarios are counted and listed but never change the status: a skip
# is a declared gap, not a failure. It is reported unconditionally, including
# when it is zero, so "0 scenarios skipped" is a claim the run makes out loud
# rather than a silence the reader has to interpret.
assert_summary() {
  local out="$1" total failed skipped
  total="$(wc -l < "$ASSERTLOG" | tr -d ' ')"
  failed="$(grep -c '^FAIL' "$ASSERTLOG" || true)"
  skipped="$(wc -l < "$SKIPLOG" | tr -d ' ')"
  {
    printf '\n## Assertion summary\n\n'
    printf -- '- assertions run: %s\n' "$total"
    printf -- '- failed: %s\n' "$failed"
    printf -- '- scenarios skipped: %s\n' "$skipped"
    if [ "$failed" -gt 0 ]; then
      printf '\n```text\n'
      grep '^FAIL' "$ASSERTLOG"
      printf '```\n'
    fi
    if [ "$skipped" -gt 0 ]; then
      printf '\n```text\n'
      cat "$SKIPLOG"
      printf '```\n'
    fi
  } >> "$out"
  printf '\nassertions: %s run, %s failed; %s scenario%s skipped\n' \
    "$total" "$failed" "$skipped" "$([ "$skipped" -eq 1 ] || echo s)"
  [ "$failed" -eq 0 ]
}
