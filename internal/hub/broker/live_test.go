package broker

import (
	"log/slog"
	"os"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestLiveMintAgainstRealAPIServer mints through a real TokenRequest endpoint
// and then uses the result.
//
// Run it with `just verify-mint`, which builds the cluster and fixtures it
// needs. It is skipped by default because it requires a live cluster, and it
// exists because the fake clients used everywhere else cannot check the two
// things that matter most here: that the API server accepts the lifetime asked
// for, and that the credential which comes back is bounded by the service
// account's RBAC rather than by the hub's.
func TestLiveMintAgainstRealAPIServer(t *testing.T) {
	if os.Getenv("CELLCAST_LIVE") != "1" {
		t.Skip("set CELLCAST_LIVE=1 with a kubeconfig for a throwaway cluster")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}

	scheme := testScheme(t)
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	// The Cluster is passed in directly (placement would have chosen it), but
	// the TrustConfig is resolved from the API server, so it has to be there.
	trust := testTrust("live-trust")
	if err := k8s.Create(t.Context(), trust); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating trust config: %v", err)
	}
	t.Cleanup(func() { _ = k8s.Delete(t.Context(), trust) })

	cluster := testCluster("live-cell", map[string]string{"env": "prod"})
	cluster.Spec.TrustConfigRef.Name = "live-trust"

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	connector := NewSecretConnector(k8s, testNamespace, cfg)
	b := New(k8s, testNamespace, time.Hour, log, NewKubernetesProvider(connector.Connect))

	cred, res, err := b.Mint(t.Context(), cluster, nil, 0, "repo:ethan-kane-ops/cellcast")
	if err != nil {
		t.Fatalf("Mint() = %v, want nil", err)
	}

	t.Logf("granted=%s clamped=%v expires_in=%s",
		res.Granted, res.Clamped, time.Until(cred.ExpiresAt).Round(time.Second))

	if res.Granted != 10*time.Minute {
		t.Errorf("granted %s, want the 10m production default", res.Granted)
	}
	if lifetime := time.Until(cred.ExpiresAt); lifetime < 9*time.Minute || lifetime > 11*time.Minute {
		t.Errorf("token lifetime %s, want about 10m", lifetime.Round(time.Second))
	}

	// The token has to actually work, and has to be the identity that was
	// asked for rather than whatever the hub itself is.
	asCaller := rest.AnonymousClientConfig(cfg)
	asCaller.BearerToken = cred.Token
	cs, err := kubernetes.NewForConfig(asCaller)
	if err != nil {
		t.Fatalf("building a client from the minted token: %v", err)
	}

	who, err := cs.AuthenticationV1().SelfSubjectReviews().Create(t.Context(),
		&authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("the minted token does not authenticate: %v", err)
	}
	if got, want := who.Status.UserInfo.Username, "system:serviceaccount:apps:deployer"; got != want {
		t.Errorf("token authenticates as %q, want %q", got, want)
	}

	// And it has to be bounded by that account's RBAC rather than the hub's.
	for _, tc := range []struct {
		ns    string
		verb  string
		res   string
		allow bool
	}{
		{ns: "apps", verb: "list", res: "pods", allow: true},
		{ns: "kube-system", verb: "list", res: "secrets", allow: false},
	} {
		review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(t.Context(),
			&authorizationv1.SelfSubjectAccessReview{
				Spec: authorizationv1.SelfSubjectAccessReviewSpec{
					ResourceAttributes: &authorizationv1.ResourceAttributes{
						Namespace: tc.ns, Verb: tc.verb, Resource: tc.res,
					},
				},
			}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("access review: %v", err)
		}
		if review.Status.Allowed != tc.allow {
			t.Errorf("%s %s in %s allowed=%v, want %v", tc.verb, tc.res, tc.ns, review.Status.Allowed, tc.allow)
		}
	}
}
