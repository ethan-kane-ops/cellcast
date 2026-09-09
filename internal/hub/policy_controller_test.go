package hub

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

func policyFixture(name string, permitted map[string]string) *cellcastv1alpha1.PlacementPolicy {
	return &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Generation: 1},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects: []cellcastv1alpha1.SubjectSelector{{
				Issuer: "https://token.actions.githubusercontent.com",
				Claims: map[string]string{"repository": "example/app"},
			}},
			PermittedCells: metav1.LabelSelector{MatchLabels: permitted},
		},
	}
}

func policyClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatalf("NewScheme() = %v, want nil", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&cellcastv1alpha1.Cluster{}, &cellcastv1alpha1.PlacementPolicy{}).
		WithObjects(objs...).
		Build()
}

func labelledCluster(name string, labels map[string]string) *cellcastv1alpha1.Cluster {
	cl := clusterFixture(name, 1, cellcastv1alpha1.ClusterStateLive)
	cl.Labels = labels
	return cl
}

// TestPolicyReportsItsMatchCount covers the failure this controller exists for:
// a selector that matches nothing is a deny-everything policy that looks
// correct in Git, and the only other symptom is deploys failing later.
func TestPolicyReportsItsMatchCount(t *testing.T) {
	tests := []struct {
		name       string
		permitted  map[string]string
		clusters   []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "selector matches a cell",
			permitted:  map[string]string{"env": "dev"},
			clusters:   []client.Object{labelledCluster("c1", map[string]string{"env": "dev"})},
			wantStatus: metav1.ConditionTrue,
			wantReason: PolicyReasonCellsMatched,
		},
		{
			name:       "selector matches nothing",
			permitted:  map[string]string{"env": "staging"},
			clusters:   []client.Object{labelledCluster("c1", map[string]string{"env": "dev"})},
			wantStatus: metav1.ConditionFalse,
			wantReason: PolicyReasonNoCellsMatched,
		},
		{
			name:       "no cells registered at all",
			permitted:  map[string]string{"env": "dev"},
			wantStatus: metav1.ConditionFalse,
			wantReason: PolicyReasonNoCellsMatched,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append([]client.Object{policyFixture("p", tt.permitted)}, tt.clusters...)
			k8s := policyClient(t, objs...)
			r := &PlacementPolicyReconciler{Client: k8s}
			key := types.NamespacedName{Namespace: testNamespace, Name: "p"}

			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile() = %v, want nil", err)
			}

			var got cellcastv1alpha1.PlacementPolicy
			if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
				t.Fatalf("Get() = %v, want nil", err)
			}

			cond := meta.FindStatusCondition(got.Status.Conditions, PolicyConditionReady)
			if cond == nil {
				t.Fatalf("conditions = %+v, want a %s condition", got.Status.Conditions, PolicyConditionReady)
			}
			if cond.Status != tt.wantStatus {
				t.Errorf("condition status = %q, want %q", cond.Status, tt.wantStatus)
			}
			if cond.Reason != tt.wantReason {
				t.Errorf("condition reason = %q, want %q", cond.Reason, tt.wantReason)
			}
			if got.Status.ObservedGeneration != 1 {
				t.Errorf("observedGeneration = %d, want 1", got.Status.ObservedGeneration)
			}
		})
	}
}

// TestPolicyStatusFollowsTheFleet pins the reason the controller watches
// Clusters. Registering or relabelling a cell changes what every policy
// matches, and a count that only moved when the policy itself was edited would
// be wrong most of the time.
func TestPolicyStatusFollowsTheFleet(t *testing.T) {
	k8s := policyClient(t, policyFixture("p", map[string]string{"env": "dev"}))
	r := &PlacementPolicyReconciler{Client: k8s}
	key := types.NamespacedName{Namespace: testNamespace, Name: "p"}

	readiness := func() metav1.ConditionStatus {
		t.Helper()
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile() = %v, want nil", err)
		}
		var got cellcastv1alpha1.PlacementPolicy
		if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
			t.Fatalf("Get() = %v, want nil", err)
		}
		return meta.FindStatusCondition(got.Status.Conditions, PolicyConditionReady).Status
	}

	if got := readiness(); got != metav1.ConditionFalse {
		t.Fatalf("readiness with no cells = %q, want %q", got, metav1.ConditionFalse)
	}

	if err := k8s.Create(t.Context(), labelledCluster("c1", map[string]string{"env": "dev"})); err != nil {
		t.Fatalf("Create() = %v, want nil", err)
	}
	if got := readiness(); got != metav1.ConditionTrue {
		t.Errorf("readiness after registering a matching cell = %q, want %q", got, metav1.ConditionTrue)
	}

	// Relabelling the cell out of the policy's reach must move it back.
	var cl cellcastv1alpha1.Cluster
	if err := k8s.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "c1"}, &cl); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	cl.Labels = map[string]string{"env": "prd"}
	if err := k8s.Update(t.Context(), &cl); err != nil {
		t.Fatalf("Update() = %v, want nil", err)
	}
	if got := readiness(); got != metav1.ConditionFalse {
		t.Errorf("readiness after relabelling the cell away = %q, want %q", got, metav1.ConditionFalse)
	}
}

func TestPolicyReconcilerIsIdempotent(t *testing.T) {
	k8s := policyClient(t,
		policyFixture("p", map[string]string{"env": "dev"}),
		labelledCluster("c1", map[string]string{"env": "dev"}),
	)
	r := &PlacementPolicyReconciler{Client: k8s}
	key := types.NamespacedName{Namespace: testNamespace, Name: "p"}

	for i := range 3 {
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile() call %d = %v, want nil", i, err)
		}
	}

	var got cellcastv1alpha1.PlacementPolicy
	if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}
	if got.ResourceVersion != "1000" {
		t.Errorf("resourceVersion = %q, want a single write past the initial object", got.ResourceVersion)
	}
}

// TestPolicyMapFuncEnqueuesEveryPolicy pins the fan-out the Clusters watch
// depends on.
func TestPolicyMapFuncEnqueuesEveryPolicy(t *testing.T) {
	k8s := policyClient(t,
		policyFixture("a", map[string]string{"env": "dev"}),
		policyFixture("b", map[string]string{"env": "prd"}),
	)
	r := &PlacementPolicyReconciler{Client: k8s}

	got := r.policiesInNamespace(t.Context(), labelledCluster("c1", map[string]string{"env": "dev"}))
	if len(got) != 2 {
		t.Fatalf("policiesInNamespace() = %+v, want both policies", got)
	}
}

func TestPolicyReconcilerIgnoresDeleted(t *testing.T) {
	r := &PlacementPolicyReconciler{Client: policyClient(t)}
	key := types.NamespacedName{Namespace: testNamespace, Name: "gone"}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile() = %v, want nil for a missing policy", err)
	}
}

// policyWithIssuers is a policy whose subject selectors name exactly these
// issuers, one selector each.
func policyWithIssuers(name string, issuers ...string) *cellcastv1alpha1.PlacementPolicy {
	policy := policyFixture(name, map[string]string{"env": "dev"})
	policy.Spec.Subjects = nil
	for _, issuer := range issuers {
		policy.Spec.Subjects = append(policy.Spec.Subjects, cellcastv1alpha1.SubjectSelector{
			Issuer: issuer,
			Claims: map[string]string{"repository": "example/app"},
		})
	}
	return policy
}

const githubIssuer = "https://token.actions.githubusercontent.com"

// TestPolicyReportsWhetherTheHubTrustsItsIssuers covers the failure that
// reaches the operator furthest from its cause. A policy naming an issuer the
// hub was never started with is valid YAML, passes the CRD's CEL rules, and can
// never match anybody; the only other symptom is a deploy refused with NoPolicy
// on somebody else's machine, days later.
func TestPolicyReportsWhetherTheHubTrustsItsIssuers(t *testing.T) {
	tests := []struct {
		name        string
		trusted     []string
		issuers     []string
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage []string
	}{
		{
			name:       "every issuer is one the hub verifies",
			trusted:    []string{githubIssuer},
			issuers:    []string{githubIssuer},
			wantStatus: metav1.ConditionTrue,
			wantReason: PolicyReasonIssuersTrusted,
		},
		{
			name:        "one selector names an issuer the hub does not have",
			trusted:     []string{githubIssuer},
			issuers:     []string{githubIssuer, "https://gitlab.example.com"},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  PolicyReasonIssuerNotTrusted,
			wantMessage: []string{"gitlab.example.com", githubIssuer},
		},
		{
			// The one an operator cannot see by eye, and the reason the message
			// quotes every issuer it names. Placement compares iss literally,
			// so these are two different issuers and the policy matches nobody.
			name:        "a trailing slash is a different issuer",
			trusted:     []string{githubIssuer},
			issuers:     []string{githubIssuer + "/"},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  PolicyReasonIssuerNotTrusted,
			wantMessage: []string{`"` + githubIssuer + `/"`, `"` + githubIssuer + `"`},
		},
		{
			name:        "the hub was started with no issuers",
			issuers:     []string{githubIssuer},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  PolicyReasonNoIssuersConfigured,
			wantMessage: []string{"--oidc-issuer"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := policyClient(t, policyWithIssuers("p", tt.issuers...),
				labelledCluster("c1", map[string]string{"env": "dev"}))
			r := &PlacementPolicyReconciler{Client: k8s, TrustedIssuers: tt.trusted}
			key := types.NamespacedName{Namespace: testNamespace, Name: "p"}

			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile() = %v, want nil", err)
			}

			var got cellcastv1alpha1.PlacementPolicy
			if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
				t.Fatalf("Get() = %v, want nil", err)
			}

			cond := meta.FindStatusCondition(got.Status.Conditions, PolicyConditionIssuerTrusted)
			if cond == nil {
				t.Fatalf("conditions = %+v, want a %s condition", got.Status.Conditions, PolicyConditionIssuerTrusted)
			}
			if cond.Status != tt.wantStatus || cond.Reason != tt.wantReason {
				t.Errorf("%s = %s/%s, want %s/%s", PolicyConditionIssuerTrusted,
					cond.Status, cond.Reason, tt.wantStatus, tt.wantReason)
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(cond.Message, want) {
					t.Errorf("message %q does not name %s, which is what the operator has to change", cond.Message, want)
				}
			}
		})
	}
}

func TestTheIssuerConditionDoesNotReplaceTheReadyOne(t *testing.T) {
	// They answer different questions and are fixed in different files. A
	// policy whose selector matches cells and whose issuer is unknown has to
	// report both, or the half that is working hides the half that is not.
	k8s := policyClient(t, policyWithIssuers("p", "https://gitlab.example.com"),
		labelledCluster("c1", map[string]string{"env": "dev"}))
	r := &PlacementPolicyReconciler{Client: k8s, TrustedIssuers: []string{githubIssuer}}
	key := types.NamespacedName{Namespace: testNamespace, Name: "p"}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}

	var got cellcastv1alpha1.PlacementPolicy
	if err := k8s.Get(t.Context(), client.ObjectKey(key), &got); err != nil {
		t.Fatalf("Get() = %v, want nil", err)
	}

	ready := meta.FindStatusCondition(got.Status.Conditions, PolicyConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v, want True: the selector does match a cell", ready)
	}
	issuer := meta.FindStatusCondition(got.Status.Conditions, PolicyConditionIssuerTrusted)
	if issuer == nil || issuer.Status != metav1.ConditionFalse {
		t.Errorf("%s = %+v, want False: the hub cannot verify that issuer", PolicyConditionIssuerTrusted, issuer)
	}
}
