package awsec2query

import (
	"fmt"
	"net/url"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/rds"
)

// Input shapes covering each list serialisation: EC2 Prefix.N, ELBv2
// Prefix.member.N, RDS locationNameList, and deep nesting under a struct.
var fuzzTargets = []func() any{
	func() any { return &ec2.DescribeInstancesInput{} },
	func() any { return &ec2.RunInstancesInput{} },
	func() any { return &ec2.CreateLaunchTemplateInput{} },
	func() any { return &elbv2.RegisterTargetsInput{} },
	func() any { return &elbv2.CreateListenerInput{} },
	func() any { return &rds.CreateDBInstanceInput{} },
}

// queryParams builds the params map the way the gateway does from a request
// body: url.ParseQuery, keeping the last value of a repeated key.
func queryParams(body string) (map[string]string, bool) {
	values, err := url.ParseQuery(body)
	if err != nil {
		return nil, false
	}
	params := make(map[string]string, len(values))
	for key, vs := range values {
		params[key] = vs[len(vs)-1]
	}
	return params, true
}

// countSliceElems tallies materialised slice elements by nesting depth,
// failing on any slice past MaxSliceLen.
func countSliceElems(t *testing.T, v reflect.Value, depth int, counts map[int]int) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			countSliceElems(t, v.Elem(), depth, counts)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				countSliceElems(t, v.Field(i), depth, counts)
			}
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}
		if v.Len() > MaxSliceLen {
			t.Fatalf("slice of %d entries exceeds MaxSliceLen %d", v.Len(), MaxSliceLen)
		}
		counts[depth] += v.Len()
		for i := range v.Len() {
			countSliceElems(t, v.Index(i), depth+1, counts)
		}
	}
}

// FuzzQueryParamsToStruct pins the properties the parser owes untrusted input:
// it never panics, and on success every list level holds no more entries than
// the request had parameters, so allocation is bounded by the input.
func FuzzQueryParamsToStruct(f *testing.F) {
	for _, seed := range []string{
		"Filter.1.Name=instance-type&Filter.1.Value.1=t2.micro&Filter.3.Name=x&Filter.3.Value.1=y",
		"Filter.2.Name=skipped&Filter.2.Value.1=x",
		"Filter.999999.Name=ignored",
		"Filter.01.Name=a&Filter.+1.Value.1=b&Filter.-1.Name=c",
		"Filter.1.Value.item.1=a&Filter.1.Value.2=b",
		"InstanceId.1=i-1&InstanceId.2=i-2&MaxResults=5&DryRun=true",
		"MaxResults=99999999999999999999&DryRun=maybe",
		"ImageId=ami-1&MinCount=1&MaxCount=1&UserData=aGVsbG8=&BlockDeviceMapping.1.DeviceName=/dev/sda1&BlockDeviceMapping.1.Ebs.VolumeSize=20",
		"TagSpecification.1.ResourceType=instance&TagSpecification.1.Tag.1.Key=k&TagSpecification.1.Tag.1.Value=v",
		"LaunchTemplateData.NetworkInterface.1.PrivateIpAddresses.1.PrivateIpAddress=10.0.0.1&LaunchTemplateData.InstanceMarketOptions.SpotOptions.ValidUntil=2026-07-10T12:00:00Z",
		"TargetGroupArn=arn&Targets.member.1.Id=i-1&Targets.member.1.Port=80&Targets.member.2.Id=i-2",
		"DefaultActions.member.1.Type=forward&DefaultActions.member.1.ForwardConfig.TargetGroups.member.1.Weight=1",
		"DBInstanceIdentifier=db&Tags.Tag.1.Key=k&Tags.Tag.1.Value=v&Tags.member.2.Key=x",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		params, ok := queryParams(body)
		if !ok {
			return
		}
		for _, target := range fuzzTargets {
			out := target()
			if err := QueryParamsToStruct(params, out); err != nil {
				continue
			}
			counts := map[int]int{}
			countSliceElems(t, reflect.ValueOf(out), 0, counts)
			for depth, n := range counts {
				if n > len(params) {
					t.Fatalf("%T: %d list entries at depth %d from %d params: %q", out, n, depth, len(params), body)
				}
			}
		}
	})
}

// FuzzQueryParamsToStruct_EntryPastGapIsIgnored pins AWS's gap rule on
// arbitrary input: adding an entry one past the first gap in any list leaves
// the parsed request unchanged.
func FuzzQueryParamsToStruct_EntryPastGapIsIgnored(f *testing.F) {
	for _, seed := range []string{
		"Filter.1.Name=a&Filter.1.Value.1=b&Filter.3.Name=c",
		"Filter.2.Name=a",
		"Filter.1.Name=a&Filter.1.Value.2=b",
		"InstanceId.1=i-1&InstanceId.3=i-3",
		"Filter.01.Name=a&Filter.1.Value.1=b",
		"Filter.1.Value.item.1=a",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		params, ok := queryParams(body)
		if !ok {
			return
		}
		want := &ec2.DescribeInstancesInput{}
		if err := QueryParamsToStruct(params, want); err != nil {
			return
		}

		pastGap := len(want.Filters) + 2
		params[fmt.Sprintf("Filter.%d.Name", pastGap)] = "past-gap"
		params[fmt.Sprintf("Filter.%d.Value.1", pastGap)] = "past-gap"
		params[fmt.Sprintf("InstanceId.%d", len(want.InstanceIds)+2)] = "past-gap"
		for i, filter := range want.Filters {
			params[fmt.Sprintf("Filter.%d.Value.%d", i+1, len(filter.Values)+2)] = "past-gap"
		}

		got := &ec2.DescribeInstancesInput{}
		if err := QueryParamsToStruct(params, got); err != nil {
			t.Fatalf("entry past the gap turned success into %v: %q", err, body)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("entry past the gap changed the parse of %q:\nwant %s\ngot  %s", body, want.String(), got.String())
		}
	})
}
