package apitest

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellcastv1alpha1 "github.com/ethan-kane-ops/cellcast/api/v1alpha1"
)

// TestClusterSchemaRefusesWhatTheMarkersForbid checks that every validation
// marker on ClusterSpec is actually enforced by the API server.
//
// The fake client used by the unit tests accepts all of these, so without this
// the markers are documentation. The endpoint and issuer patterns are the two
// that matter most: both are `^https://`, and a cell registered with a
// plaintext endpoint would have the hub minting credentials over a connection
// nobody authenticated.
func TestClusterSchemaRefusesWhatTheMarkersForbid(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	tests := []struct {
		name      string
		mutate    func(*cellcastv1alpha1.Cluster)
		wantField string
	}{
		{
			name:      "a provider outside the enum",
			mutate:    func(cl *cellcastv1alpha1.Cluster) { cl.Spec.Provider = "digitalocean" },
			wantField: "spec.provider",
		},
		{
			name:      "a state outside the enum",
			mutate:    func(cl *cellcastv1alpha1.Cluster) { cl.Spec.State = "PAUSED" },
			wantField: "spec.state",
		},
		{
			name:      "a lowercase spelling of a valid state",
			mutate:    func(cl *cellcastv1alpha1.Cluster) { cl.Spec.State = "live" },
			wantField: "spec.state",
		},
		{
			name:      "a plaintext endpoint",
			mutate:    func(cl *cellcastv1alpha1.Cluster) { cl.Spec.Endpoint = "http://cell.example.internal:6443" },
			wantField: "spec.endpoint",
		},
		{
			name:      "an endpoint that is not a URL at all",
			mutate:    func(cl *cellcastv1alpha1.Cluster) { cl.Spec.Endpoint = "cell.example.internal" },
			wantField: "spec.endpoint",
		},
		{
			name:      "an empty endpoint",
			mutate:    func(cl *cellcastv1alpha1.Cluster) { cl.Spec.Endpoint = "" },
			wantField: "spec.endpoint",
		},
		{
			name:      "an empty trust config name",
			mutate:    func(cl *cellcastv1alpha1.Cluster) { cl.Spec.TrustConfigRef.Name = "" },
			wantField: "spec.trustConfigRef.name",
		},
		{
			name: "a plaintext reporter issuer",
			mutate: func(cl *cellcastv1alpha1.Cluster) {
				cl.Spec.Reporter = &cellcastv1alpha1.ReporterIdentity{
					Issuer:  "http://kubernetes.default.svc",
					Subject: "system:serviceaccount:cellcast-system:cellcast-agent",
				}
			},
			wantField: "spec.reporter.issuer",
		},
		{
			name: "an empty reporter subject",
			mutate: func(cl *cellcastv1alpha1.Cluster) {
				cl.Spec.Reporter = &cellcastv1alpha1.ReporterIdentity{
					Issuer:  "https://oidc.example.internal",
					Subject: "",
				}
			},
			wantField: "spec.reporter.subject",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cl := validCluster(ns, refusedName(i))
			tc.mutate(cl)

			err := c.Create(context.Background(), cl)
			if err == nil {
				t.Fatalf("the API server accepted %s; the marker on %s is not enforced", tc.name, tc.wantField)
			}
			if !apierrors.IsInvalid(err) {
				t.Fatalf("want an Invalid error, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error does not name %s: %v", tc.wantField, err)
			}
		})
	}
}

func refusedName(i int) string {
	return "refused-" + string(rune('a'+i))
}

// TestClusterSchemaAcceptsAValidCell is the other half: the table above proves
// nothing if the baseline object is itself invalid for an unrelated reason.
func TestClusterSchemaAcceptsAValidCell(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	cl := validCluster(ns, "accepted")
	cl.Spec.Reporter = &cellcastv1alpha1.ReporterIdentity{
		Issuer:  "https://oidc.cell.example.internal",
		Subject: "system:serviceaccount:cellcast-system:cellcast-agent",
	}
	if err := c.Create(context.Background(), cl); err != nil {
		t.Fatalf("the API server refused a valid cell: %v", err)
	}
}

// TestCRDDefaultsAreApplied pins the `+kubebuilder:default=` markers.
//
// These are load bearing rather than cosmetic. A Cluster that defaulted to
// anything but LIVE would silently drain a freshly registered cell, and a
// PlacementPolicy that defaulted AllowDarkTargeting to true would hand every
// caller the dark cells the field exists to withhold.
func TestCRDDefaultsAreApplied(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	ctx := context.Background()

	t.Run("a cluster with no state is LIVE", func(t *testing.T) {
		cl := validCluster(ns, "defaulted")
		if err := c.Create(ctx, cl); err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := cl.Spec.State; got != cellcastv1alpha1.ClusterStateLive {
			t.Errorf("state = %q, want %q", got, cellcastv1alpha1.ClusterStateLive)
		}
	})

	t.Run("a policy defaults to LeastLoaded and withholds dark cells", func(t *testing.T) {
		pol := &cellcastv1alpha1.PlacementPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "defaulted", Namespace: ns},
			Spec: cellcastv1alpha1.PlacementPolicySpec{
				Subjects: []cellcastv1alpha1.SubjectSelector{
					{Issuer: "https://token.actions.githubusercontent.com"},
				},
			},
		}
		if err := c.Create(ctx, pol); err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := pol.Spec.Strategy; got != cellcastv1alpha1.ScoringLeastLoaded {
			t.Errorf("strategy = %q, want %q", got, cellcastv1alpha1.ScoringLeastLoaded)
		}
		if pol.Spec.AllowDarkTargeting {
			t.Error("allowDarkTargeting defaulted to true; dark cells are opt-in")
		}
	})

	t.Run("a secret reference defaults to the kubeconfig key", func(t *testing.T) {
		tc := &cellcastv1alpha1.TrustConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "defaulted", Namespace: ns},
			Spec: cellcastv1alpha1.TrustConfigSpec{
				Provider: cellcastv1alpha1.TrustProviderKubernetes,
				CredentialSource: cellcastv1alpha1.CredentialSource{
					SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "cell-creds"},
				},
				Kubernetes: &cellcastv1alpha1.KubernetesTrust{
					ServiceAccountName: "deployer",
					Namespace:          "apps",
				},
			},
		}
		if err := c.Create(ctx, tc); err != nil {
			t.Fatalf("create: %v", err)
		}
		if got := tc.Spec.CredentialSource.SecretRef.Key; got != "kubeconfig" {
			t.Errorf("secretRef.key = %q, want kubeconfig", got)
		}
	})
}

// TestPlacementPolicySchemaRequiresASubject pins MinItems=1.
//
// A policy with no subjects matches nobody, so an empty list is always an
// operator mistake. Refusing it at apply time is the difference between a
// misconfiguration surfacing on `kubectl apply` and a pipeline being refused
// placement with NoPolicy hours later.
func TestPlacementPolicySchemaRequiresASubject(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	pol := &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "no-subjects", Namespace: ns},
		Spec:       cellcastv1alpha1.PlacementPolicySpec{Subjects: nil},
	}
	err := c.Create(context.Background(), pol)
	if err == nil {
		t.Fatal("the API server accepted a policy with no subjects")
	}
	if !strings.Contains(err.Error(), "spec.subjects") {
		t.Errorf("error does not name spec.subjects: %v", err)
	}
}

// TestPlacementPolicySchemaRejectsAnUnknownStrategy pins the scoring enum.
//
// Strategy is policy-controlled rather than caller-controlled precisely so a
// caller cannot steer itself into a cell, and an unrecognised value reaching
// the engine would have to be resolved to something at scoring time.
func TestPlacementPolicySchemaRejectsAnUnknownStrategy(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	pol := &cellcastv1alpha1.PlacementPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-strategy", Namespace: ns},
		Spec: cellcastv1alpha1.PlacementPolicySpec{
			Subjects: []cellcastv1alpha1.SubjectSelector{{Issuer: "https://issuer.example"}},
			Strategy: "Random",
		},
	}
	err := c.Create(context.Background(), pol)
	if err == nil {
		t.Fatal("the API server accepted an unknown scoring strategy")
	}
	if !strings.Contains(err.Error(), "spec.strategy") {
		t.Errorf("error does not name spec.strategy: %v", err)
	}
}

// TestTrustConfigCELRulesAreEnforced covers the two XValidation rules.
//
// CEL rules are the one part of the schema with no Go equivalent anywhere in
// the repo, so nothing else in the test suite can catch a rule that was
// written backwards. Both are about the hub's own identity in a spoke, which
// makes a rule that silently accepts an under-specified config a credential
// problem rather than a usability one.
func TestTrustConfigCELRulesAreEnforced(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)

	kube := func() *cellcastv1alpha1.KubernetesTrust {
		return &cellcastv1alpha1.KubernetesTrust{ServiceAccountName: "deployer", Namespace: "apps"}
	}

	tests := []struct {
		name    string
		spec    cellcastv1alpha1.TrustConfigSpec
		wantMsg string
	}{
		{
			name: "the kubernetes provider without a kubernetes block",
			spec: cellcastv1alpha1.TrustConfigSpec{
				Provider:         cellcastv1alpha1.TrustProviderKubernetes,
				CredentialSource: cellcastv1alpha1.CredentialSource{InCluster: true},
			},
			wantMsg: "spec.kubernetes is required when provider is kubernetes",
		},
		{
			name: "both an in-cluster identity and a secret reference",
			spec: cellcastv1alpha1.TrustConfigSpec{
				Provider: cellcastv1alpha1.TrustProviderKubernetes,
				CredentialSource: cellcastv1alpha1.CredentialSource{
					InCluster: true,
					SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "cell-creds"},
				},
				Kubernetes: kube(),
			},
			wantMsg: "exactly one of inCluster or secretRef must be set",
		},
		{
			name: "neither an in-cluster identity nor a secret reference",
			spec: cellcastv1alpha1.TrustConfigSpec{
				Provider:         cellcastv1alpha1.TrustProviderKubernetes,
				CredentialSource: cellcastv1alpha1.CredentialSource{},
				Kubernetes:       kube(),
			},
			wantMsg: "exactly one of inCluster or secretRef must be set",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := &cellcastv1alpha1.TrustConfig{
				ObjectMeta: metav1.ObjectMeta{Name: refusedName(i), Namespace: ns},
				Spec:       tc.spec,
			}
			err := c.Create(context.Background(), obj)
			if err == nil {
				t.Fatalf("the API server accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("want the rule message %q, got: %v", tc.wantMsg, err)
			}
		})
	}

	accepted := []struct {
		name string
		spec cellcastv1alpha1.TrustConfigSpec
	}{
		{
			name: "an in-cluster identity alone",
			spec: cellcastv1alpha1.TrustConfigSpec{
				Provider:         cellcastv1alpha1.TrustProviderKubernetes,
				CredentialSource: cellcastv1alpha1.CredentialSource{InCluster: true},
				Kubernetes:       kube(),
			},
		},
		{
			name: "a secret reference alone",
			spec: cellcastv1alpha1.TrustConfigSpec{
				Provider: cellcastv1alpha1.TrustProviderKubernetes,
				CredentialSource: cellcastv1alpha1.CredentialSource{
					SecretRef: &cellcastv1alpha1.SecretKeyReference{Name: "cell-creds"},
				},
				Kubernetes: kube(),
			},
		},
		{
			name: "the aws provider with no kubernetes block",
			spec: cellcastv1alpha1.TrustConfigSpec{
				Provider:         cellcastv1alpha1.TrustProviderAWS,
				CredentialSource: cellcastv1alpha1.CredentialSource{InCluster: true},
			},
		},
	}

	for i, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			obj := &cellcastv1alpha1.TrustConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "accepted-" + string(rune('a'+i)), Namespace: ns},
				Spec:       tc.spec,
			}
			if err := c.Create(context.Background(), obj); err != nil {
				t.Fatalf("the API server refused %s: %v", tc.name, err)
			}
		})
	}
}

// TestStatusIsASubresource pins `+kubebuilder:subresource:status`.
//
// Without the subresource a controller writing status also writes spec, so a
// reconcile racing an operator's `kubectl patch` would revert the operator's
// state change to whatever the controller last read. The Cluster controller
// writes status on every observed transition, which makes this the resource
// where that race would actually happen.
func TestStatusIsASubresource(t *testing.T) {
	c := newClient(t)
	ns := newNamespace(t, c)
	ctx := context.Background()

	cl := validCluster(ns, "subresource")
	if err := c.Create(ctx, cl); err != nil {
		t.Fatalf("create: %v", err)
	}

	cl.Status.ObservedState = cellcastv1alpha1.ClusterStateDraining
	if err := c.Update(ctx, cl); err != nil {
		t.Fatalf("update: %v", err)
	}

	var got cellcastv1alpha1.Cluster
	key := client.ObjectKey{Name: "subresource", Namespace: ns}
	if err := c.Get(ctx, key, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.ObservedState != "" {
		t.Errorf("a spec update wrote status = %q; status is not a subresource", got.Status.ObservedState)
	}

	got.Status.ObservedState = cellcastv1alpha1.ClusterStateDraining
	if err := c.Status().Update(ctx, &got); err != nil {
		t.Fatalf("status update: %v", err)
	}

	var after cellcastv1alpha1.Cluster
	if err := c.Get(ctx, key, &after); err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status.ObservedState != cellcastv1alpha1.ClusterStateDraining {
		t.Errorf("observedState = %q after a status update, want DRAINING", after.Status.ObservedState)
	}
}
