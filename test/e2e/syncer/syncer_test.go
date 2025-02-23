/*
Copyright 2022 The KCP Authors.

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

package syncer

import (
	"context"
	"embed"
	"testing"
	"time"

	"github.com/kcp-dev/apimachinery/pkg/logicalcluster"
	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	kubernetesclientset "k8s.io/client-go/kubernetes"

	"github.com/kcp-dev/kcp/pkg/syncer"
	"github.com/kcp-dev/kcp/test/e2e/framework"
)

//go:embed *.yaml
var embeddedResources embed.FS

func TestSyncerLifecycle(t *testing.T) {
	t.Parallel()

	upstreamServer := framework.SharedKcpServer(t)

	t.Log("Creating an organization")
	orgClusterName := framework.NewOrganizationFixture(t, upstreamServer)

	t.Log("Creating a workspace")
	wsClusterName := framework.NewWorkspaceFixture(t, upstreamServer, orgClusterName, "Universal")

	syncerFixture := framework.NewSyncerFixture(t, &framework.SyncerFixtureConfig{
		UpstreamServer:       upstreamServer,
		WorkspaceClusterName: wsClusterName,
	})

	downstreamServer := syncerFixture.RunningServer
	downstreamConfig := downstreamServer.DefaultConfig(t)

	ctx, cancelFunc := context.WithCancel(context.Background())
	t.Cleanup(cancelFunc)

	syncerFixture.Start(t, ctx)

	upstreamConfig := upstreamServer.DefaultConfig(t)
	upstreamKubeClusterClient, err := kubernetesclientset.NewClusterForConfig(upstreamConfig)
	require.NoError(t, err)
	upstreamKubeClient := upstreamKubeClusterClient.Cluster(wsClusterName)

	t.Log("Creating upstream namespace...")
	upstreamNamespace, err := upstreamKubeClient.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-syncer",
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	downstreamKubeClient, err := kubernetesclientset.NewForConfig(downstreamConfig)
	require.NoError(t, err)

	// Determine downstream name of the namespace
	nsLocator := syncer.NamespaceLocator{LogicalCluster: logicalcluster.From(upstreamNamespace), Namespace: upstreamNamespace.Name}
	downstreamNamespaceName, err := syncer.PhysicalClusterNamespaceName(nsLocator)
	require.NoError(t, err)

	// TODO(marun) The name mapping should be defined for reuse outside of the transformName method in pkg/syncer
	serviceAccountName := "kcp-default"
	secretName := "kcp-default-token"
	configMapName := "kcp-root-ca.crt"

	t.Logf("Waiting for downstream service account %s/%s to be created...", downstreamNamespaceName, serviceAccountName)
	require.Eventually(t, func() bool {
		// TODO(marun) The name mapping should be defined for reuse outside of the transformName method in pkg/syncer
		_, err = downstreamKubeClient.CoreV1().ServiceAccounts(downstreamNamespaceName).Get(ctx, serviceAccountName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream service account %s/%s to be created: %v", downstreamNamespaceName, serviceAccountName, err)
			return false
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream service account %s/%s was not created", downstreamNamespaceName, serviceAccountName)

	t.Logf("Waiting for downstream service account secret %s/%s to be created...", downstreamNamespaceName, secretName)
	require.Eventually(t, func() bool {
		_, err = downstreamKubeClient.CoreV1().Secrets(downstreamNamespaceName).Get(ctx, secretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream service account secret %s/%s to be created: %v", downstreamNamespaceName, secretName, err)
			return false
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream service account secret %s/%s was not created", downstreamNamespaceName, secretName)

	t.Logf("Waiting for downstream configmap %s/%s to be created...", downstreamNamespaceName, configMapName)
	require.Eventually(t, func() bool {
		_, err = downstreamKubeClient.CoreV1().ConfigMaps(downstreamNamespaceName).Get(ctx, configMapName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream configmap %s/%s to be created: %v", downstreamNamespaceName, configMapName, err)
			return false
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream configmap %s/%s was not created", downstreamNamespaceName, configMapName)

	t.Log("Creating upstream deployment...")

	deploymentYAML, err := embeddedResources.ReadFile("deployment.yaml")
	require.NoError(t, err, "failed to read embedded deployment")

	var deployment *appsv1.Deployment
	err = yaml.Unmarshal(deploymentYAML, &deployment)
	require.NoError(t, err, "failed to unmarshal deployment")

	// This test created a new workspace that initially lacked support for deployments, but once the
	// workload cluster went ready (checked by the syncer fixture's Start method) the api importer
	// will have enabled deployments in the logical cluster.
	upstreamDeployment, err := upstreamKubeClient.AppsV1().Deployments(upstreamNamespace.Name).Create(ctx, deployment, metav1.CreateOptions{})
	require.NoError(t, err, "failed to create deployment")

	t.Logf("Waiting for downstream deployment %s/%s to be created...", downstreamNamespaceName, upstreamDeployment.Name)
	require.Eventually(t, func() bool {
		deployment, err = downstreamKubeClient.AppsV1().Deployments(downstreamNamespaceName).Get(ctx, upstreamDeployment.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false
		}
		if err != nil {
			t.Errorf("saw an error waiting for downstream deployment %s/%s to be created: %v", downstreamNamespaceName, upstreamDeployment.Name, err)
			return false
		}
		return true
	}, wait.ForeverTestTimeout, time.Millisecond*100, "downstream deployment %s/%s was not synced", downstreamNamespaceName, upstreamDeployment.Name)

	// TODO(marun) Check that the deployment had available replicas
	// TODO(marun) Check that the deployment was able to contact kcp
}
