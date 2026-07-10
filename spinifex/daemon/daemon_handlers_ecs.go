package daemon

import "github.com/nats-io/nats.go"

// --- Cluster ---

func (d *Daemon) handleECSCreateCluster(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.CreateCluster)
}

func (d *Daemon) handleECSDeleteCluster(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DeleteCluster)
}

func (d *Daemon) handleECSDescribeClusters(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DescribeClusters)
}

func (d *Daemon) handleECSListClusters(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.ListClusters)
}

// --- Task definition ---

func (d *Daemon) handleECSRegisterTaskDefinition(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.RegisterTaskDefinition)
}

func (d *Daemon) handleECSDeregisterTaskDefinition(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DeregisterTaskDefinition)
}

func (d *Daemon) handleECSDescribeTaskDefinition(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DescribeTaskDefinition)
}

func (d *Daemon) handleECSListTaskDefinitions(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.ListTaskDefinitions)
}

// --- Container instance ---

func (d *Daemon) handleECSRegisterContainerInstance(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.RegisterContainerInstance)
}

func (d *Daemon) handleECSDeregisterContainerInstance(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DeregisterContainerInstance)
}

func (d *Daemon) handleECSUpdateContainerInstancesState(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.UpdateContainerInstancesState)
}

func (d *Daemon) handleECSDescribeContainerInstances(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DescribeContainerInstances)
}

func (d *Daemon) handleECSListContainerInstances(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.ListContainerInstances)
}

// --- Task ---

func (d *Daemon) handleECSRunTask(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.RunTask)
}

func (d *Daemon) handleECSStartTask(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.StartTask)
}

func (d *Daemon) handleECSStopTask(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.StopTask)
}

func (d *Daemon) handleECSDescribeTasks(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DescribeTasks)
}

// --- Service ---

func (d *Daemon) handleECSCreateService(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.CreateService)
}

func (d *Daemon) handleECSUpdateService(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.UpdateService)
}

func (d *Daemon) handleECSDeleteService(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DeleteService)
}

func (d *Daemon) handleECSDescribeServices(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DescribeServices)
}

func (d *Daemon) handleECSListServices(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.ListServices)
}

func (d *Daemon) handleECSSubmitTaskStateChange(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.SubmitTaskStateChange)
}

func (d *Daemon) handleECSListTasks(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.ListTasks)
}

func (d *Daemon) handleECSPollAssignments(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.PollAssignments)
}

// --- Capacity ---

func (d *Daemon) handleECSProvisionCapacity(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.ProvisionCapacity)
}

// --- Tags ---

func (d *Daemon) handleECSTagResource(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.TagResource)
}

func (d *Daemon) handleECSUntagResource(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.UntagResource)
}

func (d *Daemon) handleECSListTagsForResource(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.ListTagsForResource)
}
