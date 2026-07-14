package handlers_ec2_launchtemplate

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fullRequestData returns a RequestLaunchTemplateData with every top-level field
// populated non-nil, covering all nested sub-types. It is the fixture for the
// drift guard: if the SDK gains a field, assertNoNilPointerFields flags the gap.
func fullRequestData() *ec2.RequestLaunchTemplateData {
	return &ec2.RequestLaunchTemplateData{
		BlockDeviceMappings: []*ec2.LaunchTemplateBlockDeviceMappingRequest{{
			DeviceName:  aws.String("/dev/sda1"),
			NoDevice:    aws.String(""),
			VirtualName: aws.String("ephemeral0"),
			Ebs: &ec2.LaunchTemplateEbsBlockDeviceRequest{
				DeleteOnTermination: aws.Bool(true),
				Encrypted:           aws.Bool(true),
				Iops:                aws.Int64(3000),
				KmsKeyId:            aws.String("kms-1"),
				SnapshotId:          aws.String("snap-1"),
				Throughput:          aws.Int64(125),
				VolumeSize:          aws.Int64(20),
				VolumeType:          aws.String("gp3"),
			},
		}},
		CapacityReservationSpecification: &ec2.LaunchTemplateCapacityReservationSpecificationRequest{
			CapacityReservationPreference: aws.String("open"),
			CapacityReservationTarget: &ec2.CapacityReservationTarget{
				CapacityReservationId:               aws.String("cr-1"),
				CapacityReservationResourceGroupArn: aws.String("arn:crg"),
			},
		},
		CpuOptions: &ec2.LaunchTemplateCpuOptionsRequest{
			AmdSevSnp:      aws.String("enabled"),
			CoreCount:      aws.Int64(2),
			ThreadsPerCore: aws.Int64(1),
		},
		CreditSpecification:   &ec2.CreditSpecificationRequest{CpuCredits: aws.String("standard")},
		DisableApiStop:        aws.Bool(true),
		DisableApiTermination: aws.Bool(true),
		EbsOptimized:          aws.Bool(true),
		ElasticGpuSpecifications: []*ec2.ElasticGpuSpecification{{
			Type: aws.String("eg1.medium"),
		}},
		ElasticInferenceAccelerators: []*ec2.LaunchTemplateElasticInferenceAccelerator{{
			Count: aws.Int64(1),
			Type:  aws.String("eia2.medium"),
		}},
		EnclaveOptions:                    &ec2.LaunchTemplateEnclaveOptionsRequest{Enabled: aws.Bool(true)},
		HibernationOptions:                &ec2.LaunchTemplateHibernationOptionsRequest{Configured: aws.Bool(true)},
		IamInstanceProfile:                &ec2.LaunchTemplateIamInstanceProfileSpecificationRequest{Arn: aws.String("arn:iam"), Name: aws.String("role")},
		ImageId:                           aws.String("ami-123"),
		InstanceInitiatedShutdownBehavior: aws.String("stop"),
		InstanceMarketOptions: &ec2.LaunchTemplateInstanceMarketOptionsRequest{
			MarketType: aws.String("spot"),
			SpotOptions: &ec2.LaunchTemplateSpotMarketOptionsRequest{
				BlockDurationMinutes:         aws.Int64(60),
				InstanceInterruptionBehavior: aws.String("terminate"),
				MaxPrice:                     aws.String("0.05"),
				SpotInstanceType:             aws.String("one-time"),
			},
		},
		InstanceRequirements: &ec2.InstanceRequirementsRequest{
			VCpuCount: &ec2.VCpuCountRangeRequest{Min: aws.Int64(2), Max: aws.Int64(8)},
			MemoryMiB: &ec2.MemoryMiBRequest{Min: aws.Int64(2048), Max: aws.Int64(16384)},
		},
		InstanceType:          aws.String("t3.micro"),
		KernelId:              aws.String("aki-1"),
		KeyName:               aws.String("key-1"),
		LicenseSpecifications: []*ec2.LaunchTemplateLicenseConfigurationRequest{{LicenseConfigurationArn: aws.String("arn:lic")}},
		MaintenanceOptions:    &ec2.LaunchTemplateInstanceMaintenanceOptionsRequest{AutoRecovery: aws.String("default")},
		MetadataOptions: &ec2.LaunchTemplateInstanceMetadataOptionsRequest{
			HttpEndpoint:            aws.String("enabled"),
			HttpProtocolIpv6:        aws.String("disabled"),
			HttpPutResponseHopLimit: aws.Int64(2),
			HttpTokens:              aws.String("required"),
			InstanceMetadataTags:    aws.String("enabled"),
		},
		Monitoring: &ec2.LaunchTemplatesMonitoringRequest{Enabled: aws.Bool(true)},
		NetworkInterfaces: []*ec2.LaunchTemplateInstanceNetworkInterfaceSpecificationRequest{{
			AssociateCarrierIpAddress: aws.Bool(false),
			AssociatePublicIpAddress:  aws.Bool(true),
			ConnectionTrackingSpecification: &ec2.ConnectionTrackingSpecificationRequest{
				TcpEstablishedTimeout: aws.Int64(60),
				UdpStreamTimeout:      aws.Int64(60),
				UdpTimeout:            aws.Int64(30),
			},
			DeleteOnTermination: aws.Bool(true),
			Description:         aws.String("primary"),
			DeviceIndex:         aws.Int64(0),
			EnaSrdSpecification: &ec2.EnaSrdSpecificationRequest{
				EnaSrdEnabled:          aws.Bool(true),
				EnaSrdUdpSpecification: &ec2.EnaSrdUdpSpecificationRequest{EnaSrdUdpEnabled: aws.Bool(true)},
			},
			Groups:                         []*string{aws.String("sg-1")},
			InterfaceType:                  aws.String("interface"),
			Ipv4PrefixCount:                aws.Int64(1),
			Ipv4Prefixes:                   []*ec2.Ipv4PrefixSpecificationRequest{{Ipv4Prefix: aws.String("10.0.0.0/28")}},
			Ipv6AddressCount:               aws.Int64(1),
			Ipv6Addresses:                  []*ec2.InstanceIpv6AddressRequest{{Ipv6Address: aws.String("::1")}},
			Ipv6PrefixCount:                aws.Int64(1),
			Ipv6Prefixes:                   []*ec2.Ipv6PrefixSpecificationRequest{{Ipv6Prefix: aws.String("2600::/64")}},
			NetworkCardIndex:               aws.Int64(0),
			NetworkInterfaceId:             aws.String("eni-1"),
			PrimaryIpv6:                    aws.Bool(false),
			PrivateIpAddress:               aws.String("10.0.0.5"),
			PrivateIpAddresses:             []*ec2.PrivateIpAddressSpecification{{Primary: aws.Bool(true), PrivateIpAddress: aws.String("10.0.0.5")}},
			SecondaryPrivateIpAddressCount: aws.Int64(1),
			SubnetId:                       aws.String("subnet-1"),
		}},
		Placement: &ec2.LaunchTemplatePlacementRequest{
			Affinity:             aws.String("default"),
			AvailabilityZone:     aws.String("az-1"),
			GroupId:              aws.String("pg-1"),
			GroupName:            aws.String("group"),
			HostId:               aws.String("host-1"),
			HostResourceGroupArn: aws.String("arn:hrg"),
			PartitionNumber:      aws.Int64(1),
			SpreadDomain:         aws.String("domain"),
			Tenancy:              aws.String("default"),
		},
		PrivateDnsNameOptions: &ec2.LaunchTemplatePrivateDnsNameOptionsRequest{
			EnableResourceNameDnsAAAARecord: aws.Bool(true),
			EnableResourceNameDnsARecord:    aws.Bool(true),
			HostnameType:                    aws.String("ip-name"),
		},
		RamDiskId:        aws.String("ari-1"),
		SecurityGroupIds: []*string{aws.String("sg-2")},
		SecurityGroups:   []*string{aws.String("default")},
		TagSpecifications: []*ec2.LaunchTemplateTagSpecificationRequest{{
			ResourceType: aws.String("instance"),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("web")}},
		}},
		UserData: aws.String("dXNlcg=="),
	}
}

// assertNoNilPointerFields fails if any top-level pointer/slice/map field is nil.
func assertNoNilPointerFields(t *testing.T, v any, ctx string) {
	t.Helper()
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	rt := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		switch f.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
			assert.Falsef(t, f.IsNil(), "%s: field %s is nil", ctx, rt.Field(i).Name)
		}
	}
}

func TestMapperRequestToResponse_NoSilentDrop(t *testing.T) {
	req := fullRequestData()
	assertNoNilPointerFields(t, req, "fixture") // fixture completeness

	resp, err := requestToResponse(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assertNoNilPointerFields(t, resp, "response")

	// Deep values across each nested family survive the round-trip.
	assert.Equal(t, int64(20), aws.Int64Value(resp.BlockDeviceMappings[0].Ebs.VolumeSize))
	assert.Equal(t, "cr-1", aws.StringValue(resp.CapacityReservationSpecification.CapacityReservationTarget.CapacityReservationId))
	assert.Equal(t, "required", aws.StringValue(resp.MetadataOptions.HttpTokens))
	assert.Equal(t, "10.0.0.0/28", aws.StringValue(resp.NetworkInterfaces[0].Ipv4Prefixes[0].Ipv4Prefix))
	assert.True(t, aws.BoolValue(resp.NetworkInterfaces[0].EnaSrdSpecification.EnaSrdUdpSpecification.EnaSrdUdpEnabled))
	assert.Equal(t, "Name", aws.StringValue(resp.TagSpecifications[0].Tags[0].Key))
	assert.Equal(t, int64(8), aws.Int64Value(resp.InstanceRequirements.VCpuCount.Max))
}

func TestMapperResponseToRunInstances_NoSilentDrop(t *testing.T) {
	resp, err := requestToResponse(fullRequestData())
	require.NoError(t, err)

	ri, err := responseToRunInstances(resp)
	require.NoError(t, err)
	require.NotNil(t, ri)

	// Every response field whose name also exists on RunInstancesInput must survive,
	// except InstanceRequirements (dropped) and the two renamed fields.
	respType := reflect.TypeOf(*resp)
	riVal := reflect.ValueOf(*ri)
	riType := riVal.Type()
	for field := range respType.Fields() {
		name := field.Name
		switch name {
		case "InstanceRequirements":
			continue // no RunInstances equivalent
		case "RamDiskId":
			assert.NotNil(t, ri.RamdiskId, "RamDiskId not remapped to RamdiskId")
			continue
		case "ElasticGpuSpecifications":
			assert.NotNil(t, ri.ElasticGpuSpecification, "ElasticGpuSpecifications not remapped")
			continue
		}
		sf, ok := riType.FieldByName(name)
		if !ok {
			t.Errorf("ResponseLaunchTemplateData.%s has no identically-named RunInstancesInput field and is not a known dropped/renamed field; responseToRunInstances would silently drop it", name)
			continue
		}
		f := riVal.FieldByIndex(sf.Index)
		switch f.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			assert.Falsef(t, f.IsNil(), "RunInstancesInput field %s dropped", name)
		}
	}

	// Renamed and nested values land correctly.
	assert.Equal(t, "ari-1", aws.StringValue(ri.RamdiskId))
	assert.Equal(t, "eg1.medium", aws.StringValue(ri.ElasticGpuSpecification[0].Type))
	assert.Equal(t, int64(20), aws.Int64Value(ri.BlockDeviceMappings[0].Ebs.VolumeSize))
	assert.Equal(t, "10.0.0.0/28", aws.StringValue(ri.NetworkInterfaces[0].Ipv4Prefixes[0].Ipv4Prefix))
	assert.Equal(t, "required", aws.StringValue(ri.MetadataOptions.HttpTokens))
}

func TestMapperNilInputs(t *testing.T) {
	resp, err := requestToResponse(nil)
	require.NoError(t, err)
	assert.Nil(t, resp)

	ri, err := responseToRunInstances(nil)
	require.NoError(t, err)
	assert.Nil(t, ri)
}

func TestMergeResponseData_PresenceReplace(t *testing.T) {
	base := &ec2.ResponseLaunchTemplateData{
		ImageId:          aws.String("ami-base"),
		InstanceType:     aws.String("t3.micro"),
		SecurityGroupIds: []*string{aws.String("sg-base")},
	}
	override := &ec2.ResponseLaunchTemplateData{
		InstanceType:     aws.String("t3.large"), // non-nil replaces
		SecurityGroupIds: []*string{},            // non-nil empty clears
		KeyName:          aws.String("key-new"),  // set where base was nil
		// ImageId nil -> inherits base
	}
	got := mergeResponseData(base, override)
	assert.Equal(t, "ami-base", aws.StringValue(got.ImageId), "nil override inherits base")
	assert.Equal(t, "t3.large", aws.StringValue(got.InstanceType), "non-nil override replaces")
	assert.Equal(t, "key-new", aws.StringValue(got.KeyName))
	require.NotNil(t, got.SecurityGroupIds, "explicit empty slice is non-nil")
	assert.Len(t, got.SecurityGroupIds, 0, "explicit empty slice clears base")
}

func TestMergeRunInstancesInput_Precedence(t *testing.T) {
	base := &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-tmpl"),
		InstanceType: aws.String("t3.micro"),
		KeyName:      aws.String("key-tmpl"),
	}
	override := &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.large"), // direct param wins
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(3),
		// ImageId, KeyName nil -> inherit template
	}
	got := mergeRunInstancesInput(base, override)
	assert.Equal(t, "ami-tmpl", aws.StringValue(got.ImageId), "template inherited when direct nil")
	assert.Equal(t, "t3.large", aws.StringValue(got.InstanceType), "direct param overrides template")
	assert.Equal(t, "key-tmpl", aws.StringValue(got.KeyName))
	assert.Equal(t, int64(1), aws.Int64Value(got.MinCount))
	assert.Equal(t, int64(3), aws.Int64Value(got.MaxCount))
}

func TestMergeRunInstancesInput_MinMaxNeverFromTemplate(t *testing.T) {
	// A template can never carry MinCount/MaxCount, but assert they come from the
	// direct request even if a base somehow did.
	base := &ec2.RunInstancesInput{MinCount: aws.Int64(9), MaxCount: aws.Int64(9)}
	override := &ec2.RunInstancesInput{MinCount: aws.Int64(2), MaxCount: aws.Int64(4)}
	got := mergeRunInstancesInput(base, override)
	assert.Equal(t, int64(2), aws.Int64Value(got.MinCount))
	assert.Equal(t, int64(4), aws.Int64Value(got.MaxCount))
}
