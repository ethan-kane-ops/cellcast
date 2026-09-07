package cli

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// kubeconfigMode is the permission a written kubeconfig gets.
//
// Owner-only. The file holds a live credential, and the default umask on a CI
// runner frequently produces world-readable files on a machine shared with
// other jobs (docs/threat-model.md T-05).
const kubeconfigMode os.FileMode = 0o600

// contextName is the context the written kubeconfig selects.
const contextName = "cellcast"

// writeKubeconfig renders a credential as a kubeconfig at path.
//
// The token reaches disk and nowhere else: it is never printed, never passed as
// an argument, and never placed in an environment variable a child process
// would inherit.
func writeKubeconfig(path string, cell string, cred *Credential) error {
	ca, err := base64.StdEncoding.DecodeString(cred.CertificateAuthorityData)
	if err != nil {
		return fmt.Errorf("decoding the cell's certificate authority: %w", err)
	}

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[cell] = &clientcmdapi.Cluster{
		Server:                   cred.Server,
		CertificateAuthorityData: ca,
	}
	cfg.AuthInfos[cred.ServiceAccount] = &clientcmdapi.AuthInfo{
		Token: cred.Token,
	}
	cfg.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:   cell,
		AuthInfo:  cred.ServiceAccount,
		Namespace: cred.Namespace,
	}
	cfg.CurrentContext = contextName

	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("rendering kubeconfig: %w", err)
	}

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	// O_EXCL is deliberate. Writing through an existing path would follow a
	// symlink an earlier job left behind, which on a shared runner is how a
	// credential ends up somewhere the next job can read it. A stale file is
	// removed first so a rerun still works, but the create itself never
	// follows anything.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the previous kubeconfig at %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, kubeconfigMode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Close()
}
