# e2e-analyze

Post-test failure-clustering action for the Go E2E suites.

## What it does

Runs after `go-junit-report` has converted each suite's stdout into
`junit-<suite>.xml`. For every JUnit file matched by `junit-glob`:

1. Parses each `<testcase>` with a `<failure>` / `<error>` child.
2. Strips ANSI escapes, then normalises dynamic noise (UUIDs, EC2 IDs,
   IPs, timestamps, request-ids, source line numbers) to produce a stable
   _signature_ per failure.
3. Picks the **earliest non-cascade** failure per suite as the likely
   root cause. "Cascade" = failure message matches a fixture-dependency
   marker (`must populate fix.`, `Should NOT be empty`,
   `Expected value not to be nil`). When every failure is a cascade
   (whole suite tripped on one upstream gap), the earliest cascade is
   the fallback root.
4. Buckets the remaining failures by signature and renders a markdown
   report. The report is appended to `$GITHUB_STEP_SUMMARY` and also
   written to `<log-dir>/analysis.md` so it travels with the uploaded
   artifact bundle.

The action **never fails the job** — exit-code semantics stay with the
existing "Fail job if any suite failed" gate.

## Inputs

| Input        | Required | Description |
|--------------|----------|-------------|
| `junit-glob` | yes      | Shell glob for the JUnit XML files (e.g. `${ARTIFACT_DIR}/junit-*.xml`). |
| `log-dir`    | yes      | Directory the rendered report is written into (`analysis.md`). |
| `title`      | no       | Heading at the top of the summary section. Default `E2E failure analysis`. |

## Outputs

None. The summary is appended to `$GITHUB_STEP_SUMMARY`; the same
content lands at `<log-dir>/analysis.md`.

## Local invocation

Outside Actions, the analyser prints to stdout instead of appending to a
summary file:

```sh
cd spinifex/.github/actions/e2e-analyze
go run . -junit-glob "/tmp/artifacts/junit-*.xml" -log-dir /tmp/artifacts
```

## Tests

Golden-file tests live in `testdata/`. Each subdir holds a scenario:

| Scenario           | Source                                    |
|--------------------|-------------------------------------------|
| `cascade-vpcs`     | Synthetic: one Phase 5 root + 15 cascades |
| `single-failure`   | Real artifact from run `26037892507`      |
| `no-failures`      | Synthetic: clean run                      |

Run normally:

```sh
go test ./.github/actions/e2e-analyze/
```

When the report format changes intentionally, regenerate the snapshots:

```sh
go test ./.github/actions/e2e-analyze/ -update
```

## Roadmap

- **Stage 1 (this action):** first-failure surfacing + cascade clustering.
- **Stage 2:** time-window correlation — slice daemon / qemu / OVS logs
  around each failure's `[t-5s, t+duration+5s]` window.
- **Stage 3:** rule-based root-cause classifier driven by
  `spinifex/tests/e2e/.failure-rules.yaml`.

See `docs/development/improvements/e2e-go-failure-analysis.md` in the
parent mulga repo.
