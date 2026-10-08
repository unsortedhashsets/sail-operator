//go:build e2e

// Copyright Istio Authors
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

package kubectl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/istio-ecosystem/sail-operator/pkg/test/project"
	"github.com/istio-ecosystem/sail-operator/tests/e2e/util/shell"
	"github.com/onsi/gomega"
)

type Kubectl struct {
	ClusterName string
	binary      string
	namespace   string
	kubeconfig  string
}

// New creates a new kubectl.Kubectl
func New() Kubectl {
	return Kubectl{}.WithBinary(os.Getenv("COMMAND"))
}

func (k Kubectl) build(cmd string) string {
	args := []string{k.binary}

	// Only append namespace if it's set
	if k.namespace != "" {
		args = append(args, k.namespace)
	}

	// Only append kubeconfig if it's set
	if k.kubeconfig != "" {
		args = append(args, k.kubeconfig)
	}

	args = append(args, cmd)

	// Join all the arguments with a space
	return strings.Join(args, " ")
}

// WithClusterName sets the cluster clusterName on this Kubectl
func (k Kubectl) WithClusterName(name string) Kubectl {
	k.ClusterName = name
	return k
}

// WithBinary returns a new Kubectl with the binary set to the given value; if the value is "", the binary is set to "kubectl"
func (k Kubectl) WithBinary(binary string) Kubectl {
	if binary == "" {
		k.binary = "kubectl"
	} else {
		k.binary = binary
	}
	return k
}

// WithNamespace returns a new Kubectl with the namespace set to the given value
func (k Kubectl) WithNamespace(ns string) Kubectl {
	if ns == "" {
		k.namespace = "--all-namespaces"
	} else {
		k.namespace = fmt.Sprintf("-n %s", ns)
	}
	return k
}

// WithKubeconfig returns a new Kubectl with kubeconfig set to the given value
func (k Kubectl) WithKubeconfig(kubeconfig string) Kubectl {
	if kubeconfig == "" {
		k.kubeconfig = ""
	} else {
		k.kubeconfig = fmt.Sprintf("--kubeconfig %s", kubeconfig)
	}
	return k
}

// CreateNamespace creates a namespace
// If the namespace already exists, it will return nil
func (k Kubectl) CreateNamespace(ns string) error {
	cmd := k.build(" create namespace " + ns)
	output, err := k.executeCommand(cmd)
	if err != nil {
		if strings.Contains(output, "AlreadyExists") {
			return nil
		}

		return fmt.Errorf("error creating namespace: %w, output: %s", err, output)
	}

	return nil
}

// CreateFromString creates a resource from the given yaml string
func (k Kubectl) CreateFromString(yamlString string) error {
	cmd := k.build(" create -f -")
	_, err := shell.ExecuteCommandWithInput(cmd, yamlString)
	if err != nil {
		return fmt.Errorf("error creating resource from yaml: %w", err)
	}
	return k.patchIstioForDualStack(yamlString)
}

// patchIstioForDualStack makes a freshly created Istio CR dual-stack on a dual-stack cluster.
// An Istio CR without ipFamilyPolicy yields a single-stack, IPv4-primary control plane even there,
// so a suite would exercise IPv4 only and still pass - indistinguishable from real coverage.
// Many suites build the CR inline rather than going through common.CreateIstio, so the hook lives
// here to cover all of them. No-op unless IP_FAMILY=dual and the document is an Istio CR.
// IPv6 single-stack needs nothing: the cluster has no IPv4 to fall back to.
func (k Kubectl) patchIstioForDualStack(yamlString string) error {
	// Loose count first, so the log can tell "this YAML has no Istio CR" apart from "it has one
	// and the strict parser below failed to see it" - the second is what silently costs coverage.
	istioDocs := countIstioDocs(yamlString)
	if istioDocs == 0 {
		return nil
	}
	k.logClusterIPFamily()

	ipFamily := os.Getenv("IP_FAMILY")
	if ipFamily != "dual" {
		LogDualStack("IP_FAMILY=%q, %d Istio CR(s) created - not patching, control plane stays single-stack", ipFamily, istioDocs)
		return nil
	}

	name, isIstio := istioCRName(yamlString)
	if !isIstio || name == "" {
		// Dual-stack cluster and an Istio CR was created, but we could not identify it, so it is
		// NOT patched and the suite would quietly run single-family. Never fail silently here.
		LogDualStack("WARNING: dual-stack cluster and %d Istio CR(s) created, but the parser "+
			"resolved isIstio=%t name=%q - NOT patched, this suite will run SINGLE-STACK:\n%s",
			istioDocs, isIstio, name, yamlString)
		return nil
	}
	if istioDocs > 1 {
		LogDualStack("WARNING: %d Istio CRs in one document but only %q is patched - the rest stay SINGLE-STACK", istioDocs, name)
	}

	patch := `{"spec":{"values":{` +
		`"pilot":{"ipFamilyPolicy":"RequireDualStack","env":{"ISTIO_DUAL_STACK":"true"}},` +
		`"meshConfig":{"defaultConfig":{"proxyMetadata":{"ISTIO_DUAL_STACK":"true"}}}}}}`
	LogDualStack("patching Istio %q with %s", name, patch)
	if err := k.Patch("istio", name, "merge", patch); err != nil {
		return fmt.Errorf("error patching Istio %q for dual-stack: %w", name, err)
	}
	// Read the values back off the live CR: the log should carry proof the patch landed, not just
	// that it was sent. A silently applied (or silently skipped) mutation is exactly what made
	// single-family runs on dual-stack clusters indistinguishable from real coverage.
	k.logIstioDualStackValues(name)
	return nil
}

// LogDualStack writes one dual-stack diagnostic line. The prefix lets a whole Jenkins console log
// be reduced to the dual-stack story with `grep '\[dual-stack\]'`.
func LogDualStack(format string, args ...any) {
	fmt.Printf("[dual-stack] "+format+"\n", args...)
}

var logClusterIPFamilyOnce sync.Once

// logClusterIPFamily prints what the suite believes it is running on, once per test process, so
// every Jenkins log opens with the answer. The first clusterNetwork CIDR is the cluster's primary
// family and therefore decides the primary ClusterIP of every Service, including istiod's.
func (k Kubectl) logClusterIPFamily() {
	logClusterIPFamilyOnce.Do(func() {
		ipFamily := os.Getenv("IP_FAMILY")
		cidrs, err := k.executeCommand(k.build(" get network cluster -o jsonpath={.spec.clusterNetwork[*].cidr}"))
		if err != nil {
			// Not OpenShift (e.g. kind), or no permission - the env var is still worth printing.
			LogDualStack("IP_FAMILY=%q; could not read network/cluster to confirm the cluster families: %v", ipFamily, err)
			return
		}
		LogDualStack("IP_FAMILY=%q; cluster pod CIDRs=%q (first CIDR is the primary family)", ipFamily, strings.TrimSpace(cidrs))
	})
}

// logIstioDualStackValues reads the three dual-stack settings back off the live Istio CR.
func (k Kubectl) logIstioDualStackValues(name string) {
	const jsonPath = `{.spec.values.pilot.ipFamilyPolicy} {.spec.values.pilot.env.ISTIO_DUAL_STACK} ` +
		`{.spec.values.meshConfig.defaultConfig.proxyMetadata.ISTIO_DUAL_STACK}`
	out, err := k.executeCommand(k.build(fmt.Sprintf(" get istio %s -o jsonpath=%q", name, jsonPath)))
	if err != nil {
		LogDualStack("WARNING: Istio %q was patched but could not be read back: %v", name, err)
		return
	}
	fields := strings.Fields(out)
	if len(fields) != 3 {
		LogDualStack("WARNING: Istio %q read back as %q - expected ipFamilyPolicy + 2 ISTIO_DUAL_STACK values, "+
			"so at least one did NOT stick", name, strings.TrimSpace(out))
		return
	}
	LogDualStack("Istio %q now has pilot.ipFamilyPolicy=%s pilot.env.ISTIO_DUAL_STACK=%s proxyMetadata.ISTIO_DUAL_STACK=%s",
		name, fields[0], fields[1], fields[2])
}

// countIstioDocs reports how many YAML documents declare a top-level Istio kind, matching loosely
// on purpose: trailing comments, quoting and extra spaces all count. istioCRName below matches
// strictly, so a disagreement between the two means the strict parser missed a CR and the suite
// is about to run single-stack without saying so. Logging only - it never selects what to patch.
func countIstioDocs(yamlString string) int {
	count := 0
	for _, doc := range strings.Split(yamlString, "\n---") {
		for _, line := range strings.Split(doc, "\n") {
			if line == "" || line[0] == ' ' || line[0] == '\t' {
				continue
			}
			key, value, found := strings.Cut(line, ":")
			if !found || strings.TrimSpace(key) != "kind" {
				continue
			}
			if comment := strings.Index(value, "#"); comment >= 0 {
				value = value[:comment]
			}
			if strings.Trim(strings.TrimSpace(value), `"'`) == "Istio" {
				count++
				break
			}
		}
	}
	return count
}

// istioCRName reports whether any document in yamlString is an Istio CR, and its metadata.name.
// Only unindented keys count as document-level, so a nested `targetRef.kind: Istio` - which every
// ZTunnel and IstioCNI CR carries - is not mistaken for an Istio resource.
func istioCRName(yamlString string) (string, bool) {
	for _, doc := range strings.Split(yamlString, "\n---") {
		isIstio, inMetadata, name := false, false, ""
		for _, line := range strings.Split(doc, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if line[0] == ' ' || line[0] == '\t' {
				if inMetadata && name == "" && strings.HasPrefix(trimmed, "name:") {
					name = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
				}
				continue
			}
			inMetadata = trimmed == "metadata:"
			if strings.HasPrefix(trimmed, "kind:") {
				isIstio = trimmed == "kind: Istio"
			}
		}
		if isIstio {
			return name, true
		}
	}
	return "", false
}

// ApplyString applies the given yaml string to the cluster
func (k Kubectl) ApplyString(yamlString string) error {
	cmd := k.build(" apply --server-side -f -")
	_, err := shell.ExecuteCommandWithInput(cmd, yamlString)
	if err != nil {
		return fmt.Errorf("error applying yaml: %w", err)
	}

	return nil
}

// Rollout performs rollout operations (restart, status) on a resource
func (k Kubectl) Rollout(action, kind, name string) error {
	var subcmd string

	switch action {
	case "restart":
		subcmd = fmt.Sprintf(" rollout restart %s/%s", kind, name)
	case "status":
		subcmd = fmt.Sprintf(" rollout status %s/%s --timeout=300s", kind, name)
	default:
		return fmt.Errorf("unsupported rollout action: %s", action)
	}

	cmd := k.build(subcmd)

	_, err := shell.ExecuteShell(cmd, "")
	if err != nil {
		return fmt.Errorf("rollout %s failed: %w", action, err)
	}

	return nil
}

// Apply applies the given yaml file to the cluster
func (k Kubectl) Apply(yamlFile string) error {
	return k.applyWithOptions("-f", yamlFile)
}

// ApplyWithLabels applies the given yaml file to the cluster with the given labels
func (k Kubectl) ApplyWithLabels(yamlFile, label string) error {
	return k.applyWithOptions(labelFlag(label), "-f", yamlFile)
}

// ApplyKustomize applies the given kustomization file to the cluster and if labels are provided, adds them as well
func (k Kubectl) ApplyKustomize(appName string, labels ...string) error {
	args := []string{"-k", getKustomizeDir(appName)}
	for _, label := range labels {
		if label != "" {
			args = append(args, labelFlag(label))
		}
	}
	return k.applyWithOptions(args...)
}

// applyWithOptions is a helper function to apply resources with specific options given as a string
func (k Kubectl) applyWithOptions(options ...string) error {
	cmd := []string{"apply"}
	cmd = append(cmd, options...)
	_, err := k.executeCommand(k.build(strings.Join(cmd, " ")))
	if err != nil {
		return fmt.Errorf("error applying resources: %w", err)
	}
	return nil
}

// DeleteFromFile deletes a resource from the given yaml file
func (k Kubectl) DeleteFromFile(yamlFile string) error {
	cmd := k.build(" delete -f " + yamlFile)
	_, err := k.executeCommand(cmd)
	if err != nil {
		return fmt.Errorf("error deleting resource from yaml: %w", err)
	}

	return nil
}

// Delete deletes a resource based on the namespace, kind and the name
func (k Kubectl) Delete(kind, name string) error {
	cmd := k.build(" delete " + kind + " " + name)
	_, err := k.executeCommand(cmd)
	if err != nil {
		return fmt.Errorf("error deleting deployment: %w", err)
	}

	return nil
}

// DeleteIgnoreNotFound deletes a resource and succeeds if the resource does not exist.
// Use this in teardown blocks where the resource may not have been created due to an earlier failure.
func (k Kubectl) DeleteIgnoreNotFound(kind, name string) error {
	cmd := k.build(" delete " + kind + " " + name + " --ignore-not-found=true")
	_, err := k.executeCommand(cmd)
	if err != nil {
		return fmt.Errorf("error deleting resource: %w", err)
	}
	return nil
}

// Patch patches a resource
func (k Kubectl) Patch(kind, name, patchType, patch string) error {
	cmd := k.build(fmt.Sprintf(" patch %s %s --type=%s -p=%q", kind, name, patchType, patch))
	_, err := k.executeCommand(cmd)
	if err != nil {
		return fmt.Errorf("error patching resource: %w", err)
	}
	return nil
}

// ForceDelete deletes a resource by removing its finalizers
func (k Kubectl) ForceDelete(kind, name string) error {
	// Not all resources have finalizers, trying to remove them returns an error here.
	// We explicitly ignore the error and attempt to delete the resource anyway.
	_ = k.Patch(kind, name, "json", `[{"op": "remove", "path": "/metadata/finalizers"}]`)
	return k.Delete(kind, name)
}

// GetYAML returns the yaml of a resource
func (k Kubectl) GetYAML(kind, name string) (string, error) {
	cmd := k.build(fmt.Sprintf(" get %s %s -o yaml", kind, name))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error getting yaml: %w, output: %s", err, output)
	}

	return output, nil
}

// GetClusterRoleNamesByLabel runs `kubectl|oc get clusterrole -l <selector> -o name` (cluster-scoped; no namespace).
func (k Kubectl) GetClusterRoleNamesByLabel(labelSelector string) (string, error) {
	cmd := k.build(fmt.Sprintf(" get clusterrole -l %q -o name", labelSelector))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error listing clusterroles: %w, output: %s", err, output)
	}

	return output, nil
}

// GetPods returns the pods of a namespace
func (k Kubectl) GetPods(args ...string) (string, error) {
	cmd := k.build(fmt.Sprintf(" get pods %s", strings.Join(args, " ")))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error getting pods: %w, output: %s", err, output)
	}

	return output, nil
}

// GetInternalIP returns the internal IP of a node
func (k Kubectl) GetInternalIP(label string) (string, error) {
	cmd := k.build(fmt.Sprintf(" get nodes -l %s -o jsonpath='{.items[0].status.addresses[?(@.type==\"InternalIP\")].address}'", label))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error getting internal IP: %w, output: %s", err, output)
	}

	return output, nil
}

func (k Kubectl) GetClusterAPIURL() (string, error) {
	cmd := k.build(" config view --minify -o jsonpath='{.clusters[0].cluster.server}'")
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error getting cluster api url: %w, output: %s", err, output)
	}

	return output, nil
}

// GetSecret returns the secret of a namespace
func (k Kubectl) GetSecret(secret string) (string, error) {
	cmd := k.build(fmt.Sprintf(" get secret %s -o yaml", secret))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error getting secret: %w, output %s", err, output)
	}

	return output, nil
}

// Exec executes a command in the pod or specific container, retrying on transient WebSocket close
// 1006 errors that occur on ARM64/Graviton2 when SPIRE ECDSA mTLS handshakes are slow.
func (k Kubectl) Exec(pod, container, command string) (string, error) {
	cmd := k.build(fmt.Sprintf(" exec %s %s -- %s", pod, containerFlag(container), command))

	var output string
	var execErr error
	// Noop fail handler: propagate the error to the caller rather than failing the test on timeout.
	g := gomega.NewGomega(func(string, ...int) {})
	g.Eventually(func() error {
		output, execErr = k.executeCommand(cmd)
		if isWebSocketCloseError(execErr) {
			return execErr // keep retrying on transient WebSocket drops
		}
		return nil // success or non-retryable error — stop
	}).WithTimeout(30 * time.Second).WithPolling(2 * time.Second).Should(gomega.Succeed())

	return output, execErr
}

// isWebSocketCloseError returns true for WebSocket close 1006 (abnormal closure) errors.
func isWebSocketCloseError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "websocket: close 1006") ||
		strings.Contains(msg, "abnormal closure")
}

// GetEvents returns the events of a namespace
func (k Kubectl) GetEvents() (string, error) {
	cmd := k.build(" get events")
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error getting events: %w, output: %s", err, output)
	}

	return output, nil
}

// Describe returns the description of a resource
func (k Kubectl) Describe(kind, name string) (string, error) {
	cmd := k.build(fmt.Sprintf(" describe %s %s", kind, name))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error describing resource: %w, output: %s", err, output)
	}

	return output, nil
}

// Logs returns the logs of a deployment
func (k Kubectl) Logs(pod string, since *time.Duration) (string, error) {
	cmd := k.build(fmt.Sprintf(" logs %s %s", pod, sinceFlag(since)))
	output, err := shell.ExecuteCommand(cmd)
	if err != nil {
		return "", err
	}
	return output, nil
}

// LogsPrevious returns the logs from the previous instance of a container
func (k Kubectl) LogsPrevious(pod string, since *time.Duration) (string, error) {
	cmd := k.build(fmt.Sprintf(" logs %s --previous %s", pod, sinceFlag(since)))
	output, err := shell.ExecuteCommand(cmd)
	if err != nil {
		return "", err
	}
	return output, nil
}

// TopPods returns resource utilization for pods in namespace
func (k Kubectl) TopPods() (string, error) {
	cmd := k.build(" top pods --no-headers")
	output, err := shell.ExecuteCommand(cmd)
	if err != nil {
		// metrics-server may not be available, don't fail
		return "", nil
	}
	return output, nil
}

// Label adds a label to the specified resource
func (k Kubectl) Label(kind, name, labelKey, labelValue string) error {
	_, err := k.executeCommand(k.build(fmt.Sprintf(" label %s %s %s=%s", kind, name, labelKey, labelValue)))
	return err
}

// LabelNamespaced adds a label to the specified resource in the specified namespace
func (k Kubectl) LabelNamespaced(kind, namespace, name, labelKey, labelValue string) error {
	_, err := k.executeCommand(k.build(fmt.Sprintf(" label %s -n %s %s %s=%s", kind, namespace, name, labelKey, labelValue)))
	return err
}

// RolloutRestart restarts a deployment using kubectl rollout restart
func (k Kubectl) RolloutRestart(resource string) (string, error) {
	cmd := k.build(fmt.Sprintf(" rollout restart %s", resource))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error restarting rollout: %w, output: %s", err, output)
	}
	return output, nil
}

// RolloutStatus waits for a rollout to complete
func (k Kubectl) RolloutStatus(resource string) (string, error) {
	cmd := k.build(fmt.Sprintf(" rollout status %s", resource))
	output, err := k.executeCommand(cmd)
	if err != nil {
		return "", fmt.Errorf("error checking rollout status: %w, output: %s", err, output)
	}
	return output, nil
}

// executeCommand handles running the command and then resets the namespace automatically
func (k Kubectl) executeCommand(cmd string) (string, error) {
	return shell.ExecuteCommand(cmd)
}

func sinceFlag(since *time.Duration) string {
	if since == nil {
		return ""
	}
	return "--since=" + since.String()
}

func labelFlag(label string) string {
	if label == "" {
		return ""
	}
	return "-l " + label
}

func containerFlag(container string) string {
	if container == "" {
		return ""
	}
	return "-c " + container
}

// getKustomizeDir returns the path to the Kustomize directory for a test application.
// The path is determined with the following priority:
// 1. App-specific environment variable (e.g., HTTPBIN_KUSTOMIZE_PATH).
// 2. Custom base path defined in CUSTOM_SAMPLES_PATH.
// 3. Default path within the project in this case will be: `tests/e2e/samples/httpbin`.
func getKustomizeDir(appName string) string {
	// If app specific environment variable is set, use it.
	if customPath := os.Getenv(strings.ToUpper(strings.ReplaceAll(appName, "-", "_") + "_KUSTOMIZE_PATH")); customPath != "" {
		return customPath
	}

	// If CUSTOM_SAMPLES_PATH is set, use it as the base path.
	if basePath := os.Getenv("CUSTOM_SAMPLES_PATH"); basePath != "" {
		return filepath.Join(basePath, appName)
	}

	return filepath.Join(project.RootDir, "tests", "e2e", "samples", appName)
}

// ApplyStringWithForceConflicts applies yaml using server-side apply and forces conflicts
func (k Kubectl) ApplyStringWithForceConflicts(yamlString string) error {
	cmd := k.build(" apply --server-side --force-conflicts -f -")
	_, err := shell.ExecuteCommandWithInput(cmd, yamlString)
	if err != nil {
		return fmt.Errorf("error applying yaml with force conflicts: %w", err)
	}
	return nil
}
