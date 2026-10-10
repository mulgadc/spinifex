package instancestate

// allowance is one recorded production consumer of raw instance state.
type allowance struct {
	File, Symbol string
	Inv          string // inventory item in docs/PACKAGE_BOUNDARY_MIGRATION.md
}

// allowlist mirrors the ADR-0007 slice 0 inventory in
// docs/PACKAGE_BOUNDARY_MIGRATION.md. It only shrinks: delete an entry as soon
// as its use is gone, and never add one to make a new consumer pass.
var allowlist = []allowance{
	{File: "spinifex/daemon/daemon.go", Symbol: "daemon.LocalStatePath", Inv: "INV-01"},
	{File: "spinifex/daemon/daemon.go", Symbol: "daemon.ReadLocalState", Inv: "INV-01"},
	{File: "spinifex/daemon/daemon.go", Symbol: "daemon.MarshalLocalState", Inv: "INV-01"},
	{File: "spinifex/daemon/daemon.go", Symbol: "daemon.WriteLocalStateBytes", Inv: "INV-01"},
	{File: "spinifex/daemon/daemon.go", Symbol: "JetStreamManager.InitKVBucket", Inv: "INV-02"},
	{File: "spinifex/daemon/daemon.go", Symbol: "JetStreamManager.InitTerminatedInstanceBucket", Inv: "INV-02"},
	{File: "spinifex/daemon/daemon.go", Symbol: "JetStreamManager.WriteNodeMarkerBestEffort", Inv: "INV-03"},
	{File: "spinifex/daemon/daemon.go", Symbol: "JetStreamManager.WriteRunningSet", Inv: "INV-03"},
	{File: "spinifex/daemon/instance_record_repair.go", Symbol: "JetStreamManager.WriteRunningSet", Inv: "INV-03"},
	{File: "spinifex/daemon/daemon.go", Symbol: "JetStreamManager.ListTerminatedInstances", Inv: "INV-04"},

	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.LoadState", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.WriteStoppedInstance", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.LoadStoppedInstance", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.DeleteStoppedInstance", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.UpdateStoppedInstance", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.ClaimStoppedInstance", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.ListStoppedInstances", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.WriteTerminatedInstance", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.UpdateTerminatedInstance", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.ListTerminatedInstances", Inv: "INV-05"},
	{File: "spinifex/daemon/vm_adapters.go", Symbol: "JetStreamManager.DeleteTerminatedInstance", Inv: "INV-05"},

	{File: "spinifex/runtime/compute/vm/migrate.go", Symbol: "JetStreamManager.WriteStoppedInstance", Inv: "INV-06"},
	{File: "spinifex/runtime/compute/vm/migrate.go", Symbol: "JetStreamManager.WriteTerminatedInstance", Inv: "INV-06"},
	{File: "spinifex/runtime/compute/vm/shutdown.go", Symbol: "JetStreamManager.WriteTerminatedInstance", Inv: "INV-06"},
	{File: "spinifex/runtime/compute/vm/orphan_qemu_reaper.go", Symbol: "JetStreamManager.ListTerminatedInstances", Inv: "INV-06"},
	{File: "spinifex/runtime/compute/vm/teardown_reaper.go", Symbol: "JetStreamManager.ListTerminatedInstances", Inv: "INV-06"},
	{File: "spinifex/runtime/compute/vm/teardown_reaper.go", Symbol: "JetStreamManager.UpdateTerminatedInstance", Inv: "INV-06"},
	{File: "spinifex/runtime/compute/vm/teardown_reaper.go", Symbol: "JetStreamManager.DeleteTerminatedInstance", Inv: "INV-06"},

	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.LoadStoppedInstance", Inv: "INV-07"},
	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.UpdateStoppedInstance", Inv: "INV-07"},
	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.ListStoppedInstances", Inv: "INV-07"},
	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.ListTerminatedInstances", Inv: "INV-07"},
	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.WriteStoppedInstance", Inv: "INV-07"},
	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.ClaimStoppedInstance", Inv: "INV-07"},
	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.WriteTerminatedInstance", Inv: "INV-07"},
	{File: "spinifex/domains/ec2/instance/service_impl.go", Symbol: "JetStreamManager.DeleteStoppedInstance", Inv: "INV-07"},

	{File: "spinifex/daemon/daemon_handlers_image.go", Symbol: "JetStreamManager.LoadStoppedInstance", Inv: "INV-08"},
	{File: "spinifex/daemon/daemon_handlers_image.go", Symbol: "JetStreamManager.UpdateStoppedInstance", Inv: "INV-08"},

	{File: "spinifex/daemon/instance_recovery.go", Symbol: "vm.InstanceRecord", Inv: "INV-10"},
	{File: "spinifex/daemon/instance_recovery.go", Symbol: "JetStreamManager.ListInstanceRecords", Inv: "INV-10"},
	{File: "spinifex/daemon/instance_recovery.go", Symbol: "JetStreamManager.ClaimRecoverableInstance", Inv: "INV-10"},
	{File: "spinifex/daemon/instance_recovery.go", Symbol: "JetStreamManager.ReleaseRecoveredInstance", Inv: "INV-10"},
	{File: "spinifex/daemon/instance_recovery.go", Symbol: "JetStreamManager.AbandonRecovery", Inv: "INV-10"},
	{File: "spinifex/daemon/instance_recovery.go", Symbol: "JetStreamManager.LoadInstanceRecord", Inv: "INV-10"},

	{File: "spinifex/daemon/dns_reconcile.go", Symbol: "daemon.InstanceRecordPrefix", Inv: "INV-11"},
	{File: "spinifex/daemon/dns_reconcile.go", Symbol: "daemon.InstanceStateBucket", Inv: "INV-11"},
	{File: "spinifex/daemon/dns_reconcile.go", Symbol: "JetStreamManager.ListInstanceRecords", Inv: "INV-11"},

	{File: "spinifex/daemon/eni_orphan_reaper.go", Symbol: "vm.InstanceRecord", Inv: "INV-12"},
	{File: "spinifex/daemon/eni_orphan_reaper.go", Symbol: "JetStreamManager.ListInstanceRecords", Inv: "INV-12"},
	{File: "spinifex/daemon/eni_orphan_reaper.go", Symbol: "JetStreamManager.ListTerminatedInstanceRecords", Inv: "INV-12"},

	{File: "spinifex/vpcd/imds_instance_state.go", Symbol: "daemon.LocalState", Inv: "INV-13"},
	{File: "spinifex/vpcd/imds_instance_state.go", Symbol: "daemon.LocalStatePath", Inv: "INV-13"},
	{File: "spinifex/vpcd/imds_instance_state.go", Symbol: "daemon.ReadLocalState", Inv: "INV-13"},
	{File: "spinifex/vpcd/imds_instance_state.go", Symbol: "daemon.InstanceStateBucket", Inv: "INV-13"},
	{File: "spinifex/vpcd/imds_instance_state.go", Symbol: "JetStreamManager.InitKVBucket", Inv: "INV-13"},
	{File: "spinifex/vpcd/imds_instance_state.go", Symbol: "vm.InstanceRecord", Inv: "INV-13"},

	{File: "spinifex/domains/ec2/guestmetadata/instance_lookup.go", Symbol: "vm.InstanceRecord", Inv: "INV-14"},
	{File: "spinifex/domains/ec2/guestmetadata/instance_lookup.go", Symbol: "vm.VMFromRecord", Inv: "INV-14"},
	{File: "spinifex/domains/ec2/guestmetadata/instance_lookup.go", Symbol: "JetStreamManager.LoadInstanceRecord", Inv: "INV-14"},

	{File: "spinifex/domains/network/reconcile/intent.go", Symbol: `literal "spinifex-instance-state"`, Inv: "INV-15"},
	{File: "spinifex/domains/network/reconcile/intent.go", Symbol: "vm.InstanceRecord", Inv: "INV-15"},

	{File: "spinifex/runtime/roles/awsgw/awsgw.go", Symbol: "daemon.InstanceStateBucket", Inv: "INV-16"},
	{File: "spinifex/runtime/roles/awsgw/awsgw.go", Symbol: "daemon.InstanceRecordPrefix", Inv: "INV-16"},
	{File: "spinifex/runtime/roles/awsgw/awsgw.go", Symbol: "vm.InstanceRecord", Inv: "INV-16"},

	{File: "spinifex/runtime/compute/cache/cache.go", Symbol: "vm.InstanceRecord", Inv: "INV-17"},
	{File: "spinifex/runtime/compute/cache/cache.go", Symbol: "vm.VMFromRecord", Inv: "INV-17"},

	{File: "spinifex/domains/admission/quota/records.go", Symbol: "vm.InstanceRecord", Inv: "INV-18"},
}
