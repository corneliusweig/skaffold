/*
Copyright 2019 The Skaffold Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package context

import (
	"sync"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// For testing
var (
	CurrentConfig = getCurrentConfig
)

var (
	kubeConfigOnce sync.Once
	kubeConfig     clientcmdapi.Config
	restConfig     *restclient.Config
)

func LoadKubeConfig(yamlKubeContext, cliKubeContext string) error {
	var err error
	kubeContext := yamlKubeContext
	if cliKubeContext != "" {
		kubeContext = cliKubeContext
	}

	kubeConfigOnce.Do(func() {
		logrus.Debugf("getting client config for kubeContext: %q", kubeContext)
		if kubeContext != "" {
			logrus.Infof("Activated kube-context %q", kubeContext)
		}

		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

		cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, &clientcmd.ConfigOverrides{
			CurrentContext: kubeContext,
		})

		if restConfig, err = getRestConfig(cfg, kubeContext); err != nil {
			return
		}

		kubeConfig, err = getKubeConfig(cfg, kubeContext)
	})

	if kubeContext != "" && kubeContext != kubeConfig.CurrentContext {
		logrus.Warn("Changing the kube-context is not supported after startup. Please restart Skaffold to take effect.")
	}
	return err
}

// GetRestClientConfig returns a REST client config for API calls against the Kubernetes API.
// If UseKubeContext was called before, the CurrentContext will be overridden.
// The kubeconfig used will be cached for the life of the skaffold process after the first call.
// If the CurrentContext is empty and the resulting config is empty, this method attempts to
// create a RESTClient with an in-cluster config.
func GetRestClientConfig() (*restclient.Config, error) {
	return restConfig, nil
}

// getCurrentConfig retrieves the kubeconfig file. If UseKubeContext was called before, the CurrentContext will be overridden.
// The result will be cached after the first call.
func getCurrentConfig() (clientcmdapi.Config, error) {
	return kubeConfig, nil
}

func getRestConfig(cfg clientcmd.ClientConfig, kctx string) (*restclient.Config, error) {
	restConfig, err := cfg.ClientConfig()
	if kctx == "" && clientcmd.IsEmptyConfig(err) {
		logrus.Debug("no kube-context set and no kubeConfig found, attempting in-cluster config")
		restConfig, err := restclient.InClusterConfig()
		return restConfig, errors.Wrap(err, "error creating REST client config in-cluster")
	}

	return restConfig, errors.Wrapf(err, "error creating REST client config for kubeContext '%s'", kctx)
}

// getKubeConfig retrieves and caches the raw kubeConfig. The cache ensures that Skaffold always works with the identical kubeconfig,
// even if it was changed on disk.
func getKubeConfig(cfg clientcmd.ClientConfig, kctx string) (clientcmdapi.Config, error) {
	rawConfig, err := cfg.RawConfig()
	if err != nil {
		return rawConfig, errors.Wrapf(err, "loading kubeconfig")
	}
	if kctx != "" {
		// RawConfig does not respect the ConfigOverrides
		rawConfig.CurrentContext = kctx
	}
	return rawConfig, nil
}
