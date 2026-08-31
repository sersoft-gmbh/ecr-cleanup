package kubehelpers

import (
	"path/filepath"

	"k8s.io/client-go/util/homedir"
)

type Config struct {
	KubeConfigPath string
	Namespace      string
}

func KubeConfigDefaultPath() string {
	if home := homedir.HomeDir(); home != "" {
		return filepath.Join(home, ".kube", "config")
	}
	return ""
}
