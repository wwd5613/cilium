// Copyright 2020 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8sTest

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/cilium/cilium/pkg/annotation"
	. "github.com/cilium/cilium/test/ginkgo-ext"
	"github.com/cilium/cilium/test/helpers"

	. "github.com/onsi/gomega"
)

var _ = Describe("K8sHubbleTest", func() {

	var (
		kubectl        *helpers.Kubectl
		ciliumFilename string

		hubblePodK8s1 string

		hubbleSelector  = "-l k8s-app=hubble-cli"
		hubbleNamespace = helpers.CiliumNamespace

		demoPath string

		app1Service    = "app1-service"
		app1Labels     = "id=app1,zgroup=testapp"
		apps           = []string{helpers.App1, helpers.App2, helpers.App3}
		prometheusPort = "9091"
	)

	addVisibilityAnnotation := func(ns, podLabels, direction, port, l4proto, l7proto string) {
		visibilityAnnotation := fmt.Sprintf("<%s/%s/%s/%s>", direction, port, l4proto, l7proto)
		By("Adding visibility annotation %s on pod with labels %s", visibilityAnnotation, podLabels)

		// Prints <node>=<ns>/<podname> for each pod the annotation was applied to
		res := kubectl.Exec(fmt.Sprintf("%s annotate pod -n %s -l %s %s=%q"+
			" -o 'jsonpath={.spec.nodeName}={.metadata.namespace}/{.metadata.name}{\"\\n\"}'",
			helpers.KubectlCmd,
			ns, app1Labels,
			annotation.ProxyVisibility, visibilityAnnotation))
		res.ExpectSuccess("adding proxy visibility annotation failed")

		// For each pod, check that the Cilium proxy-statistics contain the new annotation
		expectedProxyState := strings.ToLower(visibilityAnnotation)
		for node, podName := range res.KVOutput() {
			ciliumPod, err := kubectl.GetCiliumPodOnNode(helpers.CiliumNamespace, node)
			Expect(err).To(BeNil())

			// Extract annotation from endpoint model of pod. It does not have the l4proto, so we insert it manually.
			cmd := fmt.Sprintf("cilium endpoint get pod-name:%s"+
				" -o jsonpath='{range [*].status.policy.proxy-statistics[*]}<{.location}/{.port}/%s/{.protocol}>{\"\\n\"}{end}'",
				podName, strings.ToLower(l4proto))
			err = kubectl.CiliumExecUntilMatch(ciliumPod, cmd, expectedProxyState)
			Expect(err).To(BeNil(), "timed out waiting for endpoint to regenerate for visibility annotation")
		}
	}

	removeVisbilityAnnotation := func(ns, podLabels string) {
		By("Removing visibility annotation on pod with labels %s", app1Labels)
		res := kubectl.Exec(fmt.Sprintf("%s annotate pod -n %s -l %s %s-", helpers.KubectlCmd, ns, podLabels, annotation.ProxyVisibility))
		res.ExpectSuccess("removing proxy visibility annotation failed")
	}

	BeforeAll(func() {
		kubectl = helpers.CreateKubectl(helpers.K8s1VMName(), logger)
		ciliumFilename = helpers.TimestampFilename("cilium.yaml")

		demoPath = helpers.ManifestGet(kubectl.BasePath(), "demo.yaml")

		DeployCiliumOptionsAndDNS(kubectl, ciliumFilename, map[string]string{
			"global.hubble.cli.enabled":     "true",
			"global.hubble.metricsServer":   fmt.Sprintf(":%s", prometheusPort),
			"global.hubble.metrics.enabled": `"{dns:query;ignoreAAAA,drop,tcp,flow,port-distribution,icmp,http}"`,
		})

		err := kubectl.WaitforPods(hubbleNamespace, hubbleSelector, helpers.HelperTimeout)
		Expect(err).Should(BeNil(), "hubble-cli pods did not become ready")

		hubblePodK8s1, err = kubectl.GetHubbleClientPodOnNodeWithLabel(hubbleNamespace, helpers.K8s1)
		Expect(err).Should(BeNil(), "unable to find hubble-cli pod on %s", helpers.K8s1)
	})

	AfterFailed(func() {
		kubectl.CiliumReport(helpers.CiliumNamespace,
			"cilium endpoint list")
	})

	JustAfterEach(func() {
		kubectl.ValidateNoErrorsInLogs(CurrentGinkgoTestDescription().Duration)
	})

	AfterEach(func() {
		ExpectAllPodsTerminated(kubectl)
	})

	AfterAll(func() {
		kubectl.Delete(ciliumFilename)
		ExpectAllPodsTerminated(kubectl)
		kubectl.CloseSSHClient()
	})

	Context("Hubble Observe", func() {
		var (
			namespaceForTest string
			appPods          map[string]string
			app1ClusterIP    string
			app1Port         int
		)

		BeforeAll(func() {
			namespaceForTest = helpers.GenerateNamespaceForTest("")
			kubectl.NamespaceDelete(namespaceForTest)
			res := kubectl.NamespaceCreate(namespaceForTest)
			res.ExpectSuccess("could not create namespace")

			res = kubectl.Apply(helpers.ApplyOptions{FilePath: demoPath, Namespace: namespaceForTest})
			res.ExpectSuccess("could not create resource")

			err := kubectl.WaitforPods(namespaceForTest, "-l zgroup=testapp", helpers.HelperTimeout)
			Expect(err).Should(BeNil(), "test pods are not ready after timeout")

			appPods = helpers.GetAppPods(apps, namespaceForTest, kubectl, "id")
			app1ClusterIP, app1Port, err = kubectl.GetServiceHostPort(namespaceForTest, app1Service)
			Expect(err).To(BeNil(), "unable to find service in %q namespace", namespaceForTest)
		})

		AfterAll(func() {
			kubectl.Delete(demoPath)
			kubectl.NamespaceDelete(namespaceForTest)
		})

		It("Test L3/L4 Flow", func() {
			ctx, cancel := context.WithTimeout(context.Background(), helpers.MidCommandTimeout)
			defer cancel()
			follow := kubectl.HubbleObserveFollow(ctx, hubbleNamespace, hubblePodK8s1, fmt.Sprintf(
				"--last 1 --type trace --from-pod %s/%s --to-namespace %s --to-label %s --to-port %d",
				namespaceForTest, appPods[helpers.App2], namespaceForTest, app1Labels, app1Port))

			res := kubectl.ExecPodCmd(namespaceForTest, appPods[helpers.App2],
				helpers.CurlFail(fmt.Sprintf("http://%s/public", app1ClusterIP)))
			res.ExpectSuccess("%q cannot curl clusterIP %q", appPods[helpers.App2], app1ClusterIP)

			err := follow.WaitUntilMatchFilterLineTimeout(`{$.Type}`, "L3_L4", helpers.ShortCommandTimeout)
			Expect(err).To(BeNil(), fmt.Sprintf("hubble observe query timed out on %q", follow.OutputPrettyPrint()))

			// Basic check for L4 Prometheus metrics.
			_, nodeIP := kubectl.GetNodeInfo(helpers.K8s1)
			metricsUrl := fmt.Sprintf("%s/metrics", net.JoinHostPort(nodeIP, prometheusPort))
			res = kubectl.ExecPodCmd(hubbleNamespace, hubblePodK8s1, helpers.CurlFail(metricsUrl))
			res.ExpectSuccess("%s/%s cannot curl metrics %q", hubblePodK8s1, hubblePodK8s1, app1ClusterIP)
			res.ExpectContains(`hubble_flows_processed_total{subtype="to-endpoint",type="Trace",verdict="FORWARDED"}`)
		})

		It("Test L7 Flow", func() {
			addVisibilityAnnotation(namespaceForTest, app1Labels, "Ingress", "80", "TCP", "HTTP")
			defer removeVisbilityAnnotation(namespaceForTest, app1Labels)

			ctx, cancel := context.WithTimeout(context.Background(), helpers.MidCommandTimeout)
			defer cancel()
			follow := kubectl.HubbleObserveFollow(ctx, hubbleNamespace, hubblePodK8s1, fmt.Sprintf(
				"--last 1 --type l7 --from-pod %s/%s --to-namespace %s --to-label %s --protocol http",
				namespaceForTest, appPods[helpers.App2], namespaceForTest, app1Labels))

			res := kubectl.ExecPodCmd(namespaceForTest, appPods[helpers.App2],
				helpers.CurlFail(fmt.Sprintf("http://%s/public", app1ClusterIP)))
			res.ExpectSuccess("%q cannot curl clusterIP %q", appPods[helpers.App2], app1ClusterIP)

			err := follow.WaitUntilMatchFilterLineTimeout(`{$.Type}`, "L7", helpers.ShortCommandTimeout)
			Expect(err).To(BeNil(), fmt.Sprintf("hubble observe query timed out on %q", follow.OutputPrettyPrint()))

			// Basic check for L7 Prometheus metrics.
			_, nodeIP := kubectl.GetNodeInfo(helpers.K8s1)
			metricsUrl := fmt.Sprintf("%s/metrics", net.JoinHostPort(nodeIP, prometheusPort))
			res = kubectl.ExecPodCmd(hubbleNamespace, hubblePodK8s1, helpers.CurlFail(metricsUrl))
			res.ExpectSuccess("%s/%s cannot curl metrics %q", hubbleNamespace, hubblePodK8s1, app1ClusterIP)
			res.ExpectContains(`hubble_flows_processed_total{subtype="HTTP",type="L7",verdict="FORWARDED"}`)
		})
	})
})
