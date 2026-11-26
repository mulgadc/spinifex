#!/usr/bin/env bash
[ -n "${BASH_VERSION:-}" ] || exec bash "$0" "$@"
# Tests for ci-job-doc.py: tfvars placement, outcome docs built from recorded
# jobs-API JSON (human cancel, superseded, timeout, provision, test, skipped,
# GitHub-hosted, still running) and the job-start placement doc.
set -uo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
doc="$here/ci-job-doc.py"
data="$here/testdata/ci-job-doc"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want '$3', got '$2'"; fail=1; fi; }

cat >"$tmp/m.tfvars" <<'EOF'
nodes = [
  { name = "casuarina", wan_ip = "192.168.0.22", storage_pool = "drive2-pool" },
  # { name = "banksia", wan_ip = "192.168.0.99", storage_pool = "drive1-pool" },
  { name = "bottlebrush", wan_ip = "192.168.0.23", storage_pool = "drive1-pool" },
  { name = "ironbark", wan_ip = "192.168.0.24", storage_pool = "drive2-pool" },
]
EOF
out=$(python3 -I "$doc" placement "$tmp/m.tfvars")
check "placement hypervisors" "$(grep '^CI_HYPERVISORS=' <<<"$out")" "CI_HYPERVISORS=bottlebrush,casuarina,ironbark"
check "placement pools" "$(grep '^CI_POOLS=' <<<"$out")" "CI_POOLS=drive1-pool,drive2-pool"
check "placement host pool pairs" "$(grep '^CI_HYPERVISOR_POOLS=' <<<"$out")" "CI_HYPERVISOR_POOLS=bottlebrush:drive1-pool,casuarina:drive2-pool,ironbark:drive2-pool"
check "placement missing file prints nothing" "$(python3 -I "$doc" placement "$tmp/none.tfvars")" ""

body=$(python3 -I "$doc" finalize --dry-run \
  --jobs-json "$data/jobs.json" --annotations-json "$data/annotations.json")

# field <job_id> <key>: one field of one outcome doc, "-" when absent.
field() {
  python3 -I -c '
import json, sys
want = sys.argv[1]
for line in sys.stdin:
    d = json.loads(line).get("doc")
    if d and d["job_id"] == want:
        print(d.get(sys.argv[2], "-"))
        break
else:
    print("NODOC")
' "$1" "$2" <<<"$body"
}

check "human cancel class" "$(field 139673795583 failure_class)" "cancelled_human"
check "human cancel actor" "$(field 139673795583 cancelled_by)" "juliansommer"
check "human cancel cell" "$(field 139673795583 cell)" "multi"
check "superseded class" "$(field 139673795648 failure_class)" "cancelled_superseded"
check "failed test class" "$(field 139415372149 failure_class)" "test"
check "failed test cell" "$(field 139415372149 cell)" "24"
check "failed test runner" "$(field 139415372149 runner_name)" "ci-single-4"
check "success has no class" "$(field 139415372792 failure_class)" "-"
check "queue time" "$(field 139415372882 queue_seconds)" "3863"
check "max execution time class" "$(field 3 failure_class)" "timeout"
check "provision class" "$(field 4 failure_class)" "provision"
check "leg cell label" "$(field 4 cell)" "single-service"
infra=$(python3 -I - "$doc" <<'PY'
import importlib.util, sys
spec = importlib.util.spec_from_file_location("cjd", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
job = {"conclusion": "failure", "steps": [{"name": "Run actions/upload-artifact@v7", "conclusion": "failure"}]}
print(m.classify(job, [])[0])
PY
)
check "failed action step is not a test failure" "$infra" "other"
check "skipped job dropped" "$(field 1 job_id)" "NODOC"
check "running job dropped" "$(field 2 job_id)" "NODOC"
check "GitHub-hosted job dropped" "$(field 139673754703 job_id)" "NODOC"

action=$(python3 -I -c '
import json, sys
print(json.loads(sys.stdin.readline())["update"]["_id"])' <<<"$body")
check "doc id is run-attempt-job" "$action" "45749546525-1-139673795583"

# Job-start doc: placement only, no outcome fields.
rec=$(python3 -I -c '
import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("cjd", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
env = {"CI_JOB_ID": "7", "RUN_ID": "9", "RUN_ATTEMPT": "2", "CI_ENV": "env3",
       "CI_HYPERVISORS": "bottlebrush", "CI_POOLS": "drive2-pool",
       "RUNNER_NAME": "ci-single", "JOB_NAME": "Single-Node E2E",
       "SPINIFEX_OTEL_ATTRS": "ci.run_id=9,ci.cell=single,ci.job_id=7"}
d = m.placement_doc(env)
print(json.dumps({k: d.get(k) for k in ("job_id", "cell", "env", "hypervisors", "pools", "runner_name", "conclusion")}, sort_keys=True))
print(m.placement_doc({}))
' "$doc")
check "placement doc" "$(head -n1 <<<"$rec")" '{"cell": "single", "conclusion": null, "env": "env3", "hypervisors": ["bottlebrush"], "job_id": "7", "pools": ["drive2-pool"], "runner_name": "ci-single"}'
check "no job id, no doc" "$(tail -n1 <<<"$rec")" "None"

# Named environments: env2 spans one pool on three hosts, env3 one host.
cat >"$tmp/env2.tfvars" <<'EOF2'
nodes = [
  { name = "bottlebrush", wan_ip = "192.168.1.26", storage_pool = "drive3-pool" },
  { name = "ironbark",    wan_ip = "192.168.1.27", storage_pool = "drive3-pool" },
  { name = "casuarina",   wan_ip = "192.168.1.28", storage_pool = "drive3-pool" },
]
EOF2
cat >"$tmp/env3.tfvars" <<'EOF2'
nodes = [
  { name = "bottlebrush", wan_ip = "192.168.1.29", storage_pool = "drive2-pool" },
]
EOF2
out=$(python3 -I "$doc" placement "$tmp/env2.tfvars")
check "env2 pairs" "$(grep '^CI_HYPERVISOR_POOLS=' <<<"$out")" "CI_HYPERVISOR_POOLS=bottlebrush:drive3-pool,casuarina:drive3-pool,ironbark:drive3-pool"
out=$(python3 -I "$doc" placement "$tmp/env3.tfvars")
check "env3 pairs" "$(grep '^CI_HYPERVISOR_POOLS=' <<<"$out")" "CI_HYPERVISOR_POOLS=bottlebrush:drive2-pool"

# One key per job and attempt: placement and outcome share it, attempts differ.
ids=$(python3 -I - "$doc" <<'PY'
import importlib.util, sys
spec = importlib.util.spec_from_file_location("cjd", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
def pl(a):
    return m.doc_id(m.placement_doc({"CI_JOB_ID": "7", "RUN_ID": "9", "RUN_ATTEMPT": a}))
def oc(a):
    return m.doc_id(m.outcome_doc({"id": 7, "run_id": 9, "run_attempt": int(a), "conclusion": "success"}, [], {}))
print(pl("1") == oc("1"), pl("2") == oc("2"), pl("1") != pl("2"))
build = {"conclusion": "failure", "steps": [{"name": "Build E2E test binaries on runner", "conclusion": "failure"}]}
print(m.classify(build, [])[0])
suite = {"conclusion": "failure", "steps": [{"name": "Run E2E suites", "conclusion": "failure"}]}
print(m.classify(suite, [])[0])
body = '{"update": {"_id": "x"}}\n{"doc": {"job_id": "7", "hypervisor_pools": ["a:b"]}, "doc_as_upsert": true}\n'
print("hypervisor_pools" in m.drop_unmapped(body, set()), "hypervisor_pools" in m.drop_unmapped(body, {"hypervisor_pools"}))
PY
)
check "placement and outcome share a key per attempt" "$(sed -n 1p <<<"$ids")" "True True True"
check "failed build step is not a test failure" "$(sed -n 2p <<<"$ids")" "other"
check "failed suite step is a test failure" "$(sed -n 3p <<<"$ids")" "test"
check "pair field kept only when keyword-mapped" "$(tail -n1 <<<"$ids")" "False True"

# A timed_out job is a timeout even when the annotation lookup finds nothing.
# The write index must map the pair field as keyword before it is sent.
more=$(python3 -I - "$doc" <<'PY'
import importlib.util, sys
spec = importlib.util.spec_from_file_location("cjd", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
print(m.classify({"conclusion": "timed_out", "steps": []}, [])[0])
ok = {"idx": {"mappings": {"hypervisor_pools": {"mapping": {"hypervisor_pools": {"type": "keyword"}}}}}}
text = {"idx": {"mappings": {"hypervisor_pools": {"mapping": {"hypervisor_pools": {"type": "text"}}}}}}
print(m.field_is_keyword(ok, "hypervisor_pools"), m.field_is_keyword(text, "hypervisor_pools"), m.field_is_keyword({}, "hypervisor_pools"))
PY
)
check "timed_out without annotations" "$(sed -n 1p <<<"$more")" "timeout"
check "index mapping must be keyword" "$(sed -n 2p <<<"$more")" "True False False"

# A missing template is a skip, not a write; a runner-only job carries env none.
skip=$(python3 -I - "$doc" <<'PY'
import importlib.util, sys
spec = importlib.util.spec_from_file_location("cjd", sys.argv[1])
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
m.template_keywords = lambda sink, name: None
print(m.post_bulk("x:1", "idx", "{}\n") == m.SKIPPED, m.SKIPPED != m.WRITTEN)
PY
)
check "missing template reports skipped" "$(tail -n1 <<<"$skip")" "True True"
marked=$(CI_NO_PLACEMENT_JOBS='^cell-2[1-4] \(' python3 -I "$doc" finalize --jobs-json "$data/jobs.json" --annotations-json "$data/annotations.json" --dry-run | python3 -I -c '
import json, sys
docs = [json.loads(l)["doc"] for l in sys.stdin if "\"doc\"" in l]
print(sorted({d.get("env", "-") for d in docs}))')
check "runner-only job is marked env none" "$marked" "['-', 'none']"

exit "$fail"
