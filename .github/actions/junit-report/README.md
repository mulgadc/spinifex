# junit-report

Composite action that renders a glob of JUnit XML files into the GitHub
Actions job summary and emits one `::error` annotation per failed test case.

## Usage

```yaml
- name: Publish test summary
  if: always()
  uses: ./spinifex/.github/actions/junit-report   # or your own checkout path
  with:
    junit-glob: ${{ env.ARTIFACT_DIR }}/junit-*.xml
    title: "Go E2E results"
    # fail-on-empty: "true"   # opt-in: fail the step if the glob matches nothing
```

Outputs:

- `failures` — total `<failure>` + `<error>` counts across all matched files.
- `total`    — total `<testcase>` count across all matched files.

The action expects standard JUnit XML (e.g. emitted by
`gotestsum --junitfile`, `go-junit-report`, or pytest's `--junitxml`). Both
single `<testsuite>` and wrapping `<testsuites>` roots are accepted.

## Companion convention: `::group::` for noisy steps

Long-running shell phases (tofu apply, package installs, tar-over-ssh) should
wrap their output in `::group::name` / `::endgroup::` markers so the Actions
UI collapses them by default. Pattern used in the e2e-go / e2e-ddil workflows:

```bash
run() {
  local label="$1"; shift
  echo "::group::${label}"
  "$@" 2>&1 | tee -a "$TOFU_LOG"
  local rc=${PIPESTATUS[0]}
  echo "::endgroup::"
  return "$rc"
}
run "tofu apply" tofu apply -var-file=... -auto-approve -input=false
```

Combined: groups make the live UI readable; this action makes the run header
land on the failures without scrolling the log.
