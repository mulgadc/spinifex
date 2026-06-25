package daemon

import "github.com/nats-io/nats.go"

// --- Cluster ---

func (d *Daemon) handleECSCreateCluster(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.CreateCluster)
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

func (d *Daemon) handleECSDescribeTasks(msg *nats.Msg) {
	handleNATSRequest(msg, d.ecsService.DescribeTasks)
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
