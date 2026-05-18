## E2E failure analysis

### Suite `single`: 1 failed, 1 root cause likely

**Root cause (earliest non-cascade)**

- Test: `TestSingleNode/Phase8b_VPCSubnetE2E`
- Start: 00:41:33 (duration 200.5s)
- Error: Eventually: condition not met within 3m0s: [SSH handshake 192.168.0.202:22 never completed]
- File:  `vpc_test.go:227`

