## E2E failure analysis

### Suite `single`: 16 failed, 1 root cause likely

**Root cause (earliest non-cascade)**

- Test: `TestSingleNode/Phase5_LaunchInstance`
- Start: 12:30:00 (duration 2.5s)
- Error: Received unexpected error: InvalidParameterValue (HTTP 400)
- File:  `tests/e2e/harness/ec2helpers.go:164`

**Cascaded failures (15) — grouped by signature**

- 14× Phase 5 must populate fix.InstanceID
  - `TestSingleNode/Phase5a_pre_ClusterStats`
  - `TestSingleNode/Phase5a_Metadata`
  - `TestSingleNode/Phase5b_DescribeInstances`
  - `TestSingleNode/Phase5c_StopStart`
  - `TestSingleNode/Phase6_Volumes`
  - … and 9 more
- 1× Phase 2 must populate fix.AZName
  - `TestSingleNode/Phase8_NatGW`

