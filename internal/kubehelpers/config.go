package kubehelpers

import (
	"k8s.io/client-go/util/homedir"
	"path/filepath"
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
