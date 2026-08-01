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
  : > "$ASSERTLOG"
  trap 'rm -f "${ASSERTLOG:-}"' EXIT
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

# assert_summary <report-file> — append the roll-up to the report, print the
# totals to the console, and return 1 when anything failed. This return status is
# what makes ./chaos/run.sh a gate rather than a report.
assert_summary() {
  local out="$1" total failed
  total="$(wc -l < "$ASSERTLOG" | tr -d ' ')"
  failed="$(grep -c '^FAIL' "$ASSERTLOG" || true)"
  {
    printf '\n## Assertion summary\n\n'
    printf -- '- assertions run: %s\n' "$total"
    printf -- '- failed: %s\n' "$failed"
    if [ "$failed" -gt 0 ]; then
      printf '\n```text\n'
      grep '^FAIL' "$ASSERTLOG"
      printf '```\n'
    fi
  } >> "$out"
  printf '\nassertions: %s run, %s failed\n' "$total" "$failed"
  [ "$failed" -eq 0 ]
}
