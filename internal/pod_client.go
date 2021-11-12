/*
 * pod_client.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2018-2019 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package internal

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	fdbtypes "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta1"
	"github.com/FoundationDB/fdb-kubernetes-operator/pkg/podclient"
	"github.com/hashicorp/go-retryablehttp"
	corev1 "k8s.io/api/core/v1"
)

const (
	// MockUnreachableAnnotation defines if a Pod should be unreachable. This annotation
	// is currently only used for testing cases.
	MockUnreachableAnnotation = "foundationdb.org/mock-unreachable"
)

// realPodClient provides a client for use in real environments.
type realFdbPodClient struct {
	// Cluster is the cluster we are connecting to.
	Cluster *fdbtypes.FoundationDBCluster

	// Pod is the pod we are connecting to.
	Pod *corev1.Pod

	// useTLS indicates whether this is using a TLS connection to the sidecar.
	useTLS bool

	// tlsConfig contains the TLS configuration for the connection to the
	// sidecar.
	tlsConfig *tls.Config
}

// NewFdbPodClient builds a client for working with an FDB Pod
func NewFdbPodClient(cluster *fdbtypes.FoundationDBCluster, pod *corev1.Pod) (podclient.FdbPodClient, error) {
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("waiting for pod %s/%s/%s to be assigned an IP", cluster.Namespace, cluster.Name, pod.Name)
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == "foundationdb-kubernetes-sidecar" && !container.Ready {
			return nil, fmt.Errorf("waiting for pod %s/%s/%s to be ready", cluster.Namespace, cluster.Name, pod.Name)
		}
	}

	useTLS := podHasSidecarTLS(pod)

	var tlsConfig = &tls.Config{}
	if useTLS {
		certFile := os.Getenv("FDB_TLS_CERTIFICATE_FILE")
		keyFile := os.Getenv("FDB_TLS_KEY_FILE")
		caFile := os.Getenv("FDB_TLS_CA_FILE")

		if certFile == "" || keyFile == "" || caFile == "" {
			return nil, errors.New("missing one or more TLS env vars: FDB_TLS_CERTIFICATE_FILE, FDB_TLS_KEY_FILE or FDB_TLS_CA_FILE")
		}

		cert, err := tls.LoadX509KeyPair(
			certFile,
			keyFile,
		)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
		if os.Getenv("DISABLE_SIDECAR_TLS_CHECK") == "1" {
			tlsConfig.InsecureSkipVerify = true
		}
		certPool := x509.NewCertPool()
		caList, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		certPool.AppendCertsFromPEM(caList)
		tlsConfig.RootCAs = certPool
	}

	return &realFdbPodClient{Cluster: cluster, Pod: pod, useTLS: useTLS, tlsConfig: tlsConfig}, nil
}

// GetCluster returns the cluster associated with a client
func (client *realFdbPodClient) GetCluster() *fdbtypes.FoundationDBCluster {
	return client.Cluster
}

// GetPod returns the pod associated with a client
func (client *realFdbPodClient) GetPod() *corev1.Pod {
	return client.Pod
}

// getListenIP gets the IP address that a pod listens on.
func (client *realFdbPodClient) getListenIP() string {
	ips := GetPublicIPsForPod(client.Pod)
	if len(ips) > 0 {
		return ips[0]
	}

	return ""
}

// makeRequest submits a request to the sidecar.
func (client *realFdbPodClient) makeRequest(method string, path string) (string, error) {
	var resp *http.Response
	var err error

	protocol := "http"
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 2
	retryClient.RetryWaitMax = 1 * time.Second
	// Prevent logging
	retryClient.Logger = nil
	retryClient.CheckRetry = retryablehttp.ErrorPropagatedRetryPolicy

	if client.useTLS {
		retryClient.HTTPClient.Transport = &http.Transport{TLSClientConfig: client.tlsConfig}
		protocol = "https"
	}

	url := fmt.Sprintf("%s://%s:8080/%s", protocol, client.getListenIP(), path)
	switch method {
	case http.MethodGet:
		// We assume that a get request should be relative fast.
		retryClient.HTTPClient.Timeout = 5 * time.Second
		resp, err = retryClient.Get(url)
	case http.MethodPost:
		// A post request could take a little bit longer since we copy sometimes files.
		retryClient.HTTPClient.Timeout = 10 * time.Second
		resp, err = retryClient.Post(url, "application/json", strings.NewReader(""))
	default:
		return "", fmt.Errorf("unknown HTTP method %s", method)
	}

	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	bodyText := string(body)

	if err != nil {
		return "", err
	}

	return bodyText, nil
}

// IsPresent checks whether a file in the sidecar is present.
func (client *realFdbPodClient) IsPresent(filename string) (bool, error) {
	_, err := client.makeRequest("GET", fmt.Sprintf("check_hash/%s", filename))
	if err != nil {
		return false, err
	}

	return true, nil
}

// CheckHash checks whether a file in the sidecar has the expected contents.
func (client *realFdbPodClient) CheckHash(filename string, contents string) (bool, error) {
	response, err := client.makeRequest("GET", fmt.Sprintf("check_hash/%s", filename))
	if err != nil {
		return false, err
	}

	expectedHash := sha256.Sum256([]byte(contents))
	expectedHashString := hex.EncodeToString(expectedHash[:])
	return strings.Compare(expectedHashString, response) == 0, nil
}

// GenerateMonitorConf updates the monitor conf file for a pod
func (client *realFdbPodClient) GenerateMonitorConf() error {
	_, err := client.makeRequest("POST", "copy_monitor_conf")
	return err
}

// CopyFiles copies the files from the config map to the shared dynamic conf
// volume
func (client *realFdbPodClient) CopyFiles() error {
	_, err := client.makeRequest("POST", "copy_files")
	return err
}

// GetVariableSubstitutions gets the current keys and values that this
// process group will substitute into its monitor conf.
func (client *realFdbPodClient) GetVariableSubstitutions() (map[string]string, error) {
	contents, err := client.makeRequest("GET", "substitutions")
	if err != nil {
		return nil, err
	}
	substitutions := map[string]string{}
	err = json.Unmarshal([]byte(contents), &substitutions)
	if err != nil {
		log.Error(err, "Error deserializing pod substitutions", "responseBody", contents)
	}
	return substitutions, err
}

// MockFdbPodClient provides a mock connection to a pod
type mockFdbPodClient struct {
	Cluster *fdbtypes.FoundationDBCluster
	Pod     *corev1.Pod
}

// NewMockFdbPodClient builds a mock client for working with an FDB pod
func NewMockFdbPodClient(cluster *fdbtypes.FoundationDBCluster, pod *corev1.Pod) (podclient.FdbPodClient, error) {
	return &mockFdbPodClient{Cluster: cluster, Pod: pod}, nil
}

// GetCluster returns the cluster associated with a client
func (client *mockFdbPodClient) GetCluster() *fdbtypes.FoundationDBCluster {
	return client.Cluster
}

// GetPod returns the pod associated with a client
func (client *mockFdbPodClient) GetPod() *corev1.Pod {
	return client.Pod
}

// IsPresent checks whether a file in the sidecar is prsent.
func (client *mockFdbPodClient) IsPresent(filename string) (bool, error) {
	return true, nil
}

// CheckHash checks whether a file in the sidecar has the expected contents.
func (client *mockFdbPodClient) CheckHash(filename string, contents string) (bool, error) {
	return true, nil
}

// GenerateMonitorConf updates the monitor conf file for a pod
func (client *mockFdbPodClient) GenerateMonitorConf() error {
	return nil
}

// CopyFiles copies the files from the config map to the shared dynamic conf
// volume
func (client *mockFdbPodClient) CopyFiles() error {
	return nil
}

// UpdateDynamicFiles checks if the files in the dynamic conf volume match the
// expected contents, and tries to copy the latest files from the input volume
// if they do not.
func UpdateDynamicFiles(client podclient.FdbPodClient, filename string, contents string, updateFunc func(client podclient.FdbPodClient) error) (bool, error) {
	match := false
	var err error

	match, err = client.CheckHash(filename, contents)
	if err != nil {
		return false, err
	}

	if !match {
		err = updateFunc(client)
		if err != nil {
			return false, err
		}
		// We check this more or less instantly, maybe we should add some delay?
		match, err = client.CheckHash(filename, contents)
		if !match {
			log.Info("Waiting for config update",
				"namespace", client.GetCluster().Namespace,
				"cluster", client.GetCluster().Name,
				"pod", client.GetPod().Name,
				"file", filename)
		}

		return match, err
	}

	return true, nil
}

// CheckDynamicFilePresent waits for a file to be present in the dynamic conf
func CheckDynamicFilePresent(client podclient.FdbPodClient, filename string) (bool, error) {
	present, err := client.IsPresent(filename)

	if !present {
		log.Info("Waiting for file",
			"namespace", client.GetCluster().Namespace,
			"cluster", client.GetCluster().Name,
			"pod", client.GetPod().Name,
			"file", filename)
	}

	return present, err
}

// GetVariableSubstitutions gets the current keys and values that this
// process group will substitute into its monitor conf.
func (client *mockFdbPodClient) GetVariableSubstitutions() (map[string]string, error) {
	substitutions := map[string]string{}

	if client.Pod.Annotations != nil {
		if _, ok := client.Pod.Annotations[MockUnreachableAnnotation]; ok {
			return substitutions, &net.OpError{Op: "mock", Err: fmt.Errorf("not reachable")}
		}
	}

	ipString := GetPublicIPsForPod(client.Pod)[0]
	substitutions["FDB_PUBLIC_IP"] = ipString
	if ipString != "" {
		ip := net.ParseIP(ipString)
		if ip == nil {
			return nil, fmt.Errorf("failed to parse IP from pod: %s", ipString)
		}

		if ip.To4() == nil {
			substitutions["FDB_PUBLIC_IP"] = fmt.Sprintf("[%s]", ipString)
		}
	}

	if client.Cluster.Spec.FaultDomain.Key == "foundationdb.org/none" {
		substitutions["FDB_MACHINE_ID"] = client.Pod.Name
		substitutions["FDB_ZONE_ID"] = client.Pod.Name
	} else if client.Cluster.Spec.FaultDomain.Key == "foundationdb.org/kubernetes-cluster" {
		substitutions["FDB_MACHINE_ID"] = client.Pod.Spec.NodeName
		substitutions["FDB_ZONE_ID"] = client.Cluster.Spec.FaultDomain.Value
	} else {
		faultDomainSource := client.Cluster.Spec.FaultDomain.ValueFrom
		if faultDomainSource == "" {
			faultDomainSource = "spec.nodeName"
		}
		substitutions["FDB_MACHINE_ID"] = client.Pod.Spec.NodeName

		if faultDomainSource == "spec.nodeName" {
			substitutions["FDB_ZONE_ID"] = client.Pod.Spec.NodeName
		} else {
			return nil, fmt.Errorf("unsupported fault domain source %s", faultDomainSource)
		}
	}

	substitutions["FDB_INSTANCE_ID"] = GetProcessGroupIDFromMeta(client.Cluster, client.Pod.ObjectMeta)

	version, err := fdbtypes.ParseFdbVersion(client.Cluster.Spec.Version)
	if err != nil {
		return nil, err
	}

	if version.SupportsUsingBinariesFromMainContainer() {
		if client.Cluster.IsBeingUpgraded() {
			substitutions["BINARY_DIR"] = fmt.Sprintf("/var/dynamic-conf/bin/%s", client.Cluster.Spec.Version)
		} else {
			substitutions["BINARY_DIR"] = "/usr/bin"
		}
	}

	return substitutions, nil
}

// podHasSidecarTLS determines whether a pod currently has TLS enabled for the
// sidecar process.
func podHasSidecarTLS(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == "foundationdb-kubernetes-sidecar" {
			for _, arg := range container.Args {
				if arg == "--tls" {
					return true
				}
			}
		}
	}

	return false
}
