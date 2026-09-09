package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// node builds a Ready, schedulable node with the given allocatable.
func node(name string, cpu, mem string, pods int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
				corev1.ResourcePods:   *resource.NewQuantity(pods, resource.DecimalSI),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func requests(cpu, mem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}}
}

// pod builds a Running pod with one container.
func pod(name, cpu, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: requests(cpu, mem)}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestCollect(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	never := corev1.ContainerRestartPolicyNever

	cordoned := node("cordoned", "4", "8Gi", 110)
	cordoned.Spec.Unschedulable = true

	notReady := node("not-ready", "4", "8Gi", 110)
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse

	silent := node("silent", "4", "8Gi", 110)
	silent.Status.Conditions = nil

	// A control plane node with the standard taint. Counted: a taint is a
	// per-workload matching question, and excluding it reports every
	// single-node kind cluster as having no capacity at all.
	tainted := node("control-plane", "2", "4Gi", 110)
	tainted.Spec.Taints = []corev1.Taint{{
		Key:    "node-role.kubernetes.io/control-plane",
		Effect: corev1.TaintEffectNoSchedule,
	}}

	succeeded := pod("finished", "1", "1Gi")
	succeeded.Status.Phase = corev1.PodSucceeded
	failed := pod("crashed", "1", "1Gi")
	failed.Status.Phase = corev1.PodFailed
	pending := pod("waiting", "500m", "512Mi")
	pending.Status.Phase = corev1.PodPending

	// Sidecar: an init container that keeps running, so its request adds to
	// the regular containers rather than competing with them.
	withSidecar := pod("meshed", "1", "1Gi")
	withSidecar.Spec.InitContainers = []corev1.Container{
		{Name: "proxy", Resources: requests("100m", "128Mi"), RestartPolicy: &always},
	}

	// A one-shot init container larger than the whole rest of the pod. The pod
	// is admitted against the init container, not the sum.
	withBigInit := pod("migrating", "500m", "512Mi")
	withBigInit.Spec.InitContainers = []corev1.Container{
		{Name: "migrate", Resources: requests("4", "8Gi"), RestartPolicy: &never},
	}

	// A one-shot init container that runs after a sidecar has started, so both
	// are charged at once.
	initAfterSidecar := pod("both", "200m", "256Mi")
	initAfterSidecar.Spec.InitContainers = []corev1.Container{
		{Name: "proxy", Resources: requests("100m", "128Mi"), RestartPolicy: &always},
		{Name: "migrate", Resources: requests("1", "2Gi"), RestartPolicy: &never},
	}

	overhead := pod("kata", "1", "1Gi")
	overhead.Spec.Overhead = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("250m"),
		corev1.ResourceMemory: resource.MustParse("256Mi"),
	}

	tests := []struct {
		name  string
		nodes []*corev1.Node
		pods  []*corev1.Pod
		want  Snapshot
	}{
		{
			name:  "empty cell",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110)},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 4000,
				MemoryBytesAllocatable: 8 << 30, PodCapacity: 110,
			},
		},
		{
			name:  "allocatable sums across nodes",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110), node("b", "2", "4Gi", 55)},
			pods:  []*corev1.Pod{pod("one", "1", "1Gi"), pod("two", "500m", "512Mi")},
			want: Snapshot{
				Nodes: 2, CPUMilliAllocatable: 6000, CPUMilliCommitted: 1500,
				MemoryBytesAllocatable: 12 << 30, MemoryBytesCommitted: 1536 << 20,
				Pods: 2, PodCapacity: 165,
			},
		},
		{
			name:  "cordoned, not-ready and never-reported nodes are excluded",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110), cordoned, notReady, silent},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 4000,
				MemoryBytesAllocatable: 8 << 30, PodCapacity: 110,
			},
		},
		{
			name:  "a tainted control plane node still counts",
			nodes: []*corev1.Node{tainted},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 2000,
				MemoryBytesAllocatable: 4 << 30, PodCapacity: 110,
			},
		},
		{
			name:  "terminal pods hold nothing",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110)},
			pods:  []*corev1.Pod{succeeded, failed},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 4000,
				MemoryBytesAllocatable: 8 << 30, PodCapacity: 110,
			},
		},
		{
			name:  "a pending pod is committed",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110)},
			pods:  []*corev1.Pod{pending},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 4000, CPUMilliCommitted: 500,
				MemoryBytesAllocatable: 8 << 30, MemoryBytesCommitted: 512 << 20,
				Pods: 1, PodCapacity: 110,
			},
		},
		{
			name:  "a sidecar adds to the regular containers",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110)},
			pods:  []*corev1.Pod{withSidecar},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 4000, CPUMilliCommitted: 1100,
				MemoryBytesAllocatable: 8 << 30, MemoryBytesCommitted: 1152 << 20,
				Pods: 1, PodCapacity: 110,
			},
		},
		{
			name:  "a large init container wins over the container sum",
			nodes: []*corev1.Node{node("a", "8", "16Gi", 110)},
			pods:  []*corev1.Pod{withBigInit},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 8000, CPUMilliCommitted: 4000,
				MemoryBytesAllocatable: 16 << 30, MemoryBytesCommitted: 8 << 30,
				Pods: 1, PodCapacity: 110,
			},
		},
		{
			name:  "an init container is charged alongside a running sidecar",
			nodes: []*corev1.Node{node("a", "8", "16Gi", 110)},
			pods:  []*corev1.Pod{initAfterSidecar},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 8000, CPUMilliCommitted: 1100,
				MemoryBytesAllocatable: 16 << 30, MemoryBytesCommitted: 2176 << 20,
				Pods: 1, PodCapacity: 110,
			},
		},
		{
			name:  "pod overhead is charged on top",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110)},
			pods:  []*corev1.Pod{overhead},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 4000, CPUMilliCommitted: 1250,
				MemoryBytesAllocatable: 8 << 30, MemoryBytesCommitted: 1280 << 20,
				Pods: 1, PodCapacity: 110,
			},
		},
		{
			name:  "a pod with no requests is counted but commits nothing",
			nodes: []*corev1.Node{node("a", "4", "8Gi", 110)},
			pods: []*corev1.Pod{{
				ObjectMeta: metav1.ObjectMeta{Name: "besteffort"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}},
			want: Snapshot{
				Nodes: 1, CPUMilliAllocatable: 4000,
				MemoryBytesAllocatable: 8 << 30, Pods: 1, PodCapacity: 110,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Collect(tt.nodes, tt.pods); got != tt.want {
				t.Errorf("Collect() =\n\t%+v\nwant\n\t%+v", got, tt.want)
			}
		})
	}
}

// TestCollectPodsOnExcludedNodesStillCommit pins the interaction between the
// two filters, which is the one place they disagree.
//
// A cordoned node's allocatable is dropped while the pods still running on it
// are counted, so the cell reads as fuller than the arithmetic on a healthy
// cluster would suggest. Those pods are real load, and
// overstating utilisation steers deploys away from a cell being drained, which
// is the direction to be wrong in.
func TestCollectPodsOnExcludedNodesStillCommit(t *testing.T) {
	cordoned := node("cordoned", "4", "8Gi", 110)
	cordoned.Spec.Unschedulable = true

	got := Collect(
		[]*corev1.Node{node("healthy", "4", "8Gi", 110), cordoned},
		[]*corev1.Pod{pod("on-cordoned", "3", "6Gi")},
	)

	if got.CPUMilliAllocatable != 4000 {
		t.Errorf("CPUMilliAllocatable = %d, want 4000 (the cordoned node is excluded)", got.CPUMilliAllocatable)
	}
	if got.CPUMilliCommitted != 3000 {
		t.Errorf("CPUMilliCommitted = %d, want 3000 (its pods are not)", got.CPUMilliCommitted)
	}
}
