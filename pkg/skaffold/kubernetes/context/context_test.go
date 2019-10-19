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
	"io/ioutil"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/GoogleContainerTools/skaffold/testutil"
)

const clusterFooContext = "cluster-foo"
const clusterBarContext = "cluster-bar"

const validKubeConfig = `
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://foo.com
  name: cluster-foo
- cluster:
    server: https://bar.com
  name: cluster-bar
contexts:
- context:
    cluster: cluster-foo
    user: user1
  name: cluster-foo
- context:
    cluster: cluster-bar
    user: user1
  name: cluster-bar
current-context: cluster-foo
users:
- name: user1
  user:
    password: secret
    username: user
`

const changedKubeConfig = `
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://changed-url.com
  name: some-cluster
contexts:
- context:
    cluster: some-cluster
    user: user-bar
  name: cluster-bar
- context:
    cluster: some-cluster
    user: user-baz
  name: cluster-baz
current-context: cluster-baz
users:
- name: user1
  user:
    password: secret
    username: user
`

func TestLoadKubeConfig(t *testing.T) {
	testutil.Run(t, "invalid context", func(t *testutil.T) {
		resetKubeConfig(t, "invalid")

		err := LoadKubeConfig("", "", "")

		t.CheckError(true, err)
	})

	testutil.Run(t, "kubeconfig CLI flag takes precedence", func(t *testutil.T) {
		resetKubeConfig(t, validKubeConfig)
		kubeConfigFile := t.TempFile("config", []byte(changedKubeConfig))

		err := LoadKubeConfig("", "", kubeConfigFile)
		t.CheckNoError(err)

		cfg, _ := CurrentConfig()
		t.CheckDeepEqual("cluster-baz", cfg.CurrentContext)
	})

	testutil.Run(t, "kube-config immutability", func(t *testutil.T) {
		logrus.SetLevel(logrus.InfoLevel)
		kubeConfig := t.TempFile("config", []byte(validKubeConfig))
		kubeConfigOnce = sync.Once{}

		err := LoadKubeConfig("", clusterBarContext, kubeConfig)
		t.CheckNoError(err)

		cfg, _ := GetRestClientConfig()
		t.CheckDeepEqual("https://bar.com", cfg.Host)

		if err = ioutil.WriteFile(kubeConfig, []byte(changedKubeConfig), 0644); err != nil {
			t.Error(err)
		}

		err = LoadKubeConfig("", clusterBarContext, kubeConfig)
		t.CheckNoError(err)

		cfg, _ = GetRestClientConfig()
		t.CheckDeepEqual("https://bar.com", cfg.Host)
	})

	testutil.Run(t, "REST client in-cluster", func(t *testutil.T) {
		logrus.SetLevel(logrus.DebugLevel)
		t.SetEnvs(map[string]string{"KUBECONFIG": "non-valid"})
		kubeConfigOnce = sync.Once{}

		err := LoadKubeConfig("", "", "")

		if err == nil {
			t.Errorf("expected error outside the cluster")
		}
	})
}

func TestCurrentContext(t *testing.T) {
	testutil.Run(t, "valid context", func(t *testutil.T) {
		resetKubeConfig(t, validKubeConfig)

		err := LoadKubeConfig("", "", "")
		t.CheckNoError(err)

		cfg, _ := CurrentConfig()
		t.CheckDeepEqual(clusterFooContext, cfg.CurrentContext)
	})

	testutil.Run(t, "valid context with override", func(t *testutil.T) {
		resetKubeConfig(t, validKubeConfig)

		err := LoadKubeConfig("", clusterBarContext, "")
		t.CheckNoError(err)

		cfg, _ := CurrentConfig()
		t.CheckDeepEqual(clusterBarContext, cfg.CurrentContext)
	})
}

func TestGetRestClientConfig(t *testing.T) {
	testutil.Run(t, "valid context", func(t *testutil.T) {
		resetKubeConfig(t, validKubeConfig)

		err := LoadKubeConfig("", "", "")
		t.CheckNoError(err)

		cfg, _ := GetRestClientConfig()
		t.CheckDeepEqual("https://foo.com", cfg.Host)
	})

	testutil.Run(t, "valid context with override", func(t *testutil.T) {
		resetKubeConfig(t, validKubeConfig)

		err := LoadKubeConfig(clusterBarContext, "", "")
		t.CheckNoError(err)

		cfg, _ := GetRestClientConfig()
		t.CheckDeepEqual("https://bar.com", cfg.Host)
	})
}

func TestLoadKubeConfig_argumentPrecedence(t *testing.T) {
	type invocation struct {
		cliValue, yamlValue string
	}
	tests := []struct {
		name        string
		invocations []invocation
		expected    string
	}{
		{
			name:        "when not called at all",
			invocations: nil,
			expected:    "",
		},
		{
			name:        "yaml value when no CLI value is given",
			invocations: []invocation{{yamlValue: clusterBarContext}},
			expected:    clusterBarContext,
		},
		{
			name: "yaml value when no CLI value is given, first invocation persists",
			invocations: []invocation{
				{yamlValue: clusterBarContext},
				{yamlValue: clusterFooContext},
			},
			expected: clusterBarContext,
		},
		{
			name:        "CLI value takes precedence",
			invocations: []invocation{{cliValue: clusterBarContext, yamlValue: "context2"}},
			expected:    clusterBarContext,
		},
		{
			name: "first CLI value takes precedence",
			invocations: []invocation{
				{cliValue: clusterBarContext},
				{cliValue: clusterFooContext},
			},
			expected: clusterBarContext,
		},
		{
			name: "mixed CLI value and yaml value - I",
			invocations: []invocation{
				{cliValue: clusterBarContext},
				{yamlValue: clusterFooContext},
			},
			expected: clusterBarContext,
		},
		{
			name: "mixed CLI value and yaml value - II",
			invocations: []invocation{
				{yamlValue: clusterBarContext},
				{cliValue: clusterFooContext},
			},
			expected: clusterBarContext,
		},
		{
			name: "mixed CLI value and yaml value - III",
			invocations: []invocation{
				{yamlValue: clusterBarContext},
				{cliValue: clusterFooContext, yamlValue: "context-third"},
			},
			expected: clusterBarContext,
		},
		{
			name: "mixed CLI value and yaml value - IV",
			invocations: []invocation{
				{cliValue: clusterBarContext, yamlValue: clusterFooContext},
				{cliValue: "context-third", yamlValue: "context-fourth"},
			},
			expected: clusterBarContext,
		},
	}

	for _, test := range tests {
		testutil.Run(t, test.name, func(t *testutil.T) {
			resetKubeConfig(t, validKubeConfig)
			for _, inv := range test.invocations {
				if err := LoadKubeConfig(inv.yamlValue, inv.cliValue, ""); err != nil {
					t.Error(err)
				}
			}

			cfg, _ := CurrentConfig()
			t.CheckDeepEqual(test.expected, cfg.CurrentContext)
		})
	}
}

func resetKubeConfig(t *testutil.T, content string) {
	kubeConfigFile := t.TempFile("config", []byte(content))
	t.SetEnvs(map[string]string{"KUBECONFIG": kubeConfigFile})
	kubeConfig.CurrentContext = ""
	kubeConfigOnce = sync.Once{}
}
