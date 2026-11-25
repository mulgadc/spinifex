### Initial targets receive traffic

A target in the `initial` state receives traffic before its first successful health check.
On AWS, a target receives no traffic until it passes.

### Unused attributes

Only the `X-Forwarded-For` attributes of `routing.http.*` change the data plane.
Other `routing.http.*` attributes are stored and returned by `DescribeLoadBalancerAttributes`, but have no effect.
