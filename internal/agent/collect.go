package agent

import (
	corev1 "k8s.io/api/core/v1"
)

// Snapshot is one measurement of a cell's committed capacity.
//
// Field names carry their units for the same reason the hub's Report does: the
// two plausible readings of "cpu: 4" differ by a factor of a thousand, and a
// placement made on the wrong one is silently wrong rather than loudly broken.
type Snapshot struct {
	Nodes                  int
	CPUMilliAllocatable    int64
	CPUMilliCommitted      int64
	MemoryBytesAllocatable int64
	MemoryBytesCommitted   int64
	Pods                   int
	PodCapacity            int
}

// Collect derives a cell's capacity from its nodes and pods.
//
// A pure function over two slices so the arithmetic is testable without a
// cluster, which matters because every number here steers real deploys.
func Collect(nodes []*corev1.Node, pods []*corev1.Pod) Snapshot {
	var s Snapshot

	for _, n := range nodes {
		if !schedulable(n) {
			continue
		}
		s.Nodes++
		s.CPUMilliAllocatable += n.Status.Allocatable.Cpu().MilliValue()
		s.MemoryBytesAllocatable += n.Status.Allocatable.Memory().Value()
		s.PodCapacity += int(n.Status.Allocatable.Pods().Value())
	}

	for _, p := range pods {
		if terminal(p) {
			continue
		}
		s.Pods++
		cpu, mem := podRequests(p)
		s.CPUMilliCommitted += cpu
		s.MemoryBytesCommitted += mem
	}

	return s
}

// schedulable reports whether a node's allocatable is available to new work.
//
// Two exclusions, both because counting the node would overstate what the cell
// can actually take: a cordoned node receives nothing, and a NotReady node's
// allocatable is a number the API server last heard rather than capacity that
// exists.
//
// Taints are not an exclusion. A taint is a per-workload matching
// question and this report is a cell-level fact, so filtering on them would
// score a cluster on the subset of it that tolerates nothing. It would also
// report every single-node kind cluster as having zero capacity, because the
// control plane node carries a NoSchedule taint, which is a good reminder that
// the "obvious" filter here is wrong.
func schedulable(n *corev1.Node) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	// A node with no Ready condition has never reported. Excluding it is the
	// same call as excluding a NotReady one.
	return false
}

// terminal reports whether a pod has released its resources.
//
// Only Succeeded and Failed have. A Pending pod is counted: either it is about
// to occupy the resources it asked for, or it cannot be scheduled, and a cell
// that cannot schedule its own pending work is precisely a cell the next deploy
// should avoid. Counting it errs towards reporting the cell as fuller, which is
// the safe direction for a placement decision.
func terminal(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// podRequests returns the CPU millicores and memory bytes the scheduler
// reserves for a pod.
//
// This is the scheduler's own formula, not a sum of the container list. Two
// things make the naive sum wrong, and both are common enough to matter:
//
//   - Init containers run before the regular ones, so a pod whose init
//     container asks for more than the whole rest of the pod is admitted
//     against that larger figure.
//   - A sidecar is an init container with `restartPolicy: Always`. It keeps
//     running alongside the regular containers, so its request is added to
//     them rather than being an alternative to them. Service meshes put one in
//     every pod in the cluster, so ignoring these under-reports a whole fleet.
//
// Pod overhead is added last: it is the runtime's own cost, charged on top
// whichever branch wins.
func podRequests(p *corev1.Pod) (cpuMilli, memBytes int64) {
	for i := range p.Spec.Containers {
		c := &p.Spec.Containers[i].Resources.Requests
		cpuMilli += c.Cpu().MilliValue()
		memBytes += c.Memory().Value()
	}

	// Running total of the sidecars declared so far. An init container is
	// admitted alongside every sidecar that started before it.
	var sidecarCPU, sidecarMem int64
	var initCPU, initMem int64

	for i := range p.Spec.InitContainers {
		ic := &p.Spec.InitContainers[i]
		cpu := ic.Resources.Requests.Cpu().MilliValue()
		mem := ic.Resources.Requests.Memory().Value()

		initCPU = max(initCPU, cpu+sidecarCPU)
		initMem = max(initMem, mem+sidecarMem)

		if ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecarCPU += cpu
			sidecarMem += mem
		}
	}

	cpuMilli = max(cpuMilli+sidecarCPU, initCPU)
	memBytes = max(memBytes+sidecarMem, initMem)

	if p.Spec.Overhead != nil {
		cpuMilli += p.Spec.Overhead.Cpu().MilliValue()
		memBytes += p.Spec.Overhead.Memory().Value()
	}
	return cpuMilli, memBytes
}
