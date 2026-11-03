#!/usr/bin/env bash
# Cluster-free self-test for the nightly's credential scan. Runs the real step
# out of .github/workflows/chaos-matrix.yml, not a copy, under the shell flags
# GitHub gives it, and plants a fake credential for each pattern. Proves a refusal
# names the file, the pattern and the line, and that no matched text reaches the
# log: that log is public, and a refused run publishes no artifact, so those three
# facts are all anyone gets to judge a hit by.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

fails=0
check() {   # check <what> <actual> <want>
  if [ "$2" = "$3" ]; then
    printf 'ok     %s\n' "$1"
  else
    printf 'FAIL   %s (got %s, want %s)\n' "$1" "$2" "$3"
    fails=$((fails + 1))
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
rep="$tmp/report.md"
corpus="$tmp/corpus.jsonl"

# A block scalar ends at the first non-blank line indented less than its body.
awk '
  /^ *- name: Scan the report for credential material$/ { step = 1; next }
  step && !body && /^ *run: \|$/ { body = 1; next }
  body {
    if ($0 !~ /^          / && $0 !~ /^ *$/) exit
    sub(/^          /, "")
    print
  }
' .github/workflows/chaos-matrix.yml > "$tmp/step.tmpl"
sed -e "s|\${{ steps.report.outputs.path }}|$rep|" \
    -e "s|\${{ steps.report.outputs.corpus }}|$corpus|" \
    "$tmp/step.tmpl" > "$tmp/step.sh"
check "the step was found in the yml" "$(grep -c 'refusing to upload' "$tmp/step.sh" || true)" 1
check "no placeholder survived"       "$(grep -c '\${{' "$tmp/step.sh" || true)" 0

out=""
rc=0
run_step() {   # GitHub runs a `run:` block with no `shell:` key as `bash -e {0}`
  out="$(bash -e "$tmp/step.sh" 2>&1)" && rc=0 || rc=$?
}
has() {   # has <fixed text>: how many log lines hold it
  printf '%s\n' "$out" | grep -acF -- "$1" || true
}
exact() {   # exact <text>: how many log lines are exactly it, so that "at line 4"
  printf '%s\n' "$out" | grep -acxF -- "$1" || true   # cannot pass on "at line 4 4"
}
clean_files() {   # 3 lines of report and 2 of corpus, none matching any pattern
  printf '# results\n## Baseline\nfine: nothing: here\n' > "$rep"
  printf '{"slug":"a","rc":0}\n{"slug":"b","rc":0}\n' > "$corpus"
}

# Built from pieces so that this file does not itself match a secret scanner.
mark=PLANTEDVALUE
q16=QQQQQQQQQQQQQQQQ
fake_jwt="eyJ$q16.eyJ$q16.$mark"
fake_pem="-----BEGIN RSA PRIVATE ""KEY----- $mark"
fake_key="AKIA$q16"
example_key="AKIA""IOSFODNN7EXAMPLE"

no_value() {   # no_value <what>: nothing a pattern matched may reach the log
  local bit
  for bit in "$mark" 'eyJ' 'BEGIN' 'Bearer' 'token: ' 'client-key-data' 'client-certificate-data' 'AKIA'; do
    check "$1: the log never holds '$bit'" "$(has "$bit")" 0
  done
}

# --- a clean run -----------------------------------------------------------
clean_files
run_step
check "clean files pass"           "$rc" 0
check "clean files say so"         "$(has 'artifacts are clean')" 1
check "clean files name no line"   "$(has ' at line ')" 0

# --- each pattern, planted in the report only ------------------------------
# Line 4 of the report, and nothing in the corpus: a grep that finds nothing in
# one file must not abort the step under -e before the refusal is printed.
each() {   # each <what> <planted line>
  clean_files
  printf '%s\n' "$2" >> "$rep"
  run_step
  check "$1: refused"                   "$rc" 1
  check "$1: the refusal is printed"    "$(has 'refusing to upload')" 1
  check "$1: named at its file and line" "$(exact "  $1 in report.md at line 4")" 1
  check "$1: the clean file is not named" "$(has "$1 in corpus.jsonl")" 0
  no_value "$1"
}
each 'bearer-token shapes'   "$fake_jwt"
each 'PEM blocks'            "$fake_pem"
each 'Authorization headers' "Authorization: Bearer $mark"
each 'kubeconfig fields'     "token: $mark"
each 'kubeconfig fields'     "client-key-data: $mark"
each 'kubeconfig fields'     "client-certificate-data: $mark"
each 'unexpected AWS keys'   "$fake_key $mark"

# --- the counts stay as they were ------------------------------------------
check "the count table still counts" \
  "$(printf '%s\n' "$out" | grep -cE '^unexpected AWS keys +1$' || true)" 1

# --- both files, several lines ---------------------------------------------
clean_files
printf 'token: %s\nfine\ntoken: %s\n' "$mark" "$mark" >> "$rep"
printf 'token: %s\n' "$mark" >> "$corpus"
run_step
check "several lines: refused"         "$rc" 1
check "several lines: the report's"    "$(exact '  kubeconfig fields in report.md at line 4 6')" 1
check "several lines: the corpus's"    "$(exact '  kubeconfig fields in corpus.jsonl at line 3')" 1
no_value "several lines"

# --- the corpus alone ------------------------------------------------------
clean_files
printf 'token: %s\n' "$mark" >> "$corpus"
run_step
check "corpus only: refused"           "$rc" 1
check "corpus only: named"             "$(exact '  kubeconfig fields in corpus.jsonl at line 3')" 1
check "corpus only: the report is not" "$(has 'kubeconfig fields in report.md')" 0
no_value "corpus only"

# --- a missing corpus ------------------------------------------------------
clean_files
rm "$corpus"
printf 'token: %s\n' "$mark" >> "$rep"
run_step
check "no corpus: refused"             "$rc" 1
check "no corpus: says it skipped it"  "$(has 'skipping it')" 1
check "no corpus: the report is named" "$(exact '  kubeconfig fields in report.md at line 4')" 1
no_value "no corpus"

# --- the one AWS key scenario 10 injects on purpose ------------------------
clean_files
printf 'scenario 10 planted %s\n' "$example_key" >> "$rep"
run_step
check "the documentation key passes"   "$rc" 0
check "the documentation key is named nowhere" "$(has ' at line ')" 0

clean_files
printf '%s\nboth %s and %s\n' "$example_key" "$example_key" "$fake_key" >> "$rep"
run_step
check "a real key beside it: refused"  "$rc" 1
check "a real key beside it: only its line" "$(exact '  unexpected AWS keys in report.md at line 5')" 1
no_value "a real key beside it"

clean_files
printf '%s %s\n' "$fake_key" "$fake_key" >> "$rep"
run_step
check "two keys on one line: one number" "$(exact '  unexpected AWS keys in report.md at line 4')" 1
no_value "two keys on one line"

# --- bytes that are not text -----------------------------------------------
clean_files
printf 'token: \001\377:%s\n' "$mark" >> "$rep"
run_step
check "raw bytes: refused"             "$rc" 1
check "raw bytes: named"               "$(exact '  kubeconfig fields in report.md at line 4')" 1
no_value "raw bytes"

printf '\n%s checks failed\n' "$fails"
[ "$fails" -eq 0 ]
