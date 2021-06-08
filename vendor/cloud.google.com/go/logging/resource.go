// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logging

import (
	"io/ioutil"
	"os"
	"strings"
	"sync"

	"cloud.google.com/go/compute/metadata"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

// CommonResource sets the monitored resource associated with all log entries
// written from a Logger. If not provided, the resource is automatically
// detected based on the running environment (on GCE, GCR, GCF and GAE Standard only).
// This value can be overridden per-entry by setting an Entry's Resource field.
func CommonResource(r *mrpb.MonitoredResource) LoggerOption { return commonResource{r} }

type commonResource struct{ *mrpb.MonitoredResource }

func (r commonResource) set(l *Logger) { l.commonResource = r.MonitoredResource }

var detectedResource struct {
	pb   *mrpb.MonitoredResource
	once sync.Once
}

// isAppEngine returns true for both standard and flex
func isAppEngine() bool {
	_, service := os.LookupEnv("GAE_SERVICE")
	_, version := os.LookupEnv("GAE_VERSION")
	_, instance := os.LookupEnv("GAE_INSTANCE")

	return service && version && instance
}

func detectAppEngineResource() *mrpb.MonitoredResource {
	projectID, err := metadata.ProjectID()
	if err != nil {
		return nil
	}
	if projectID == "" {
		projectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	zone, err := metadata.Zone()
	if err != nil {
		return nil
	}

	return &mrpb.MonitoredResource{
		Type: "gae_app",
		Labels: map[string]string{
			"project_id":  projectID,
			"module_id":   os.Getenv("GAE_SERVICE"),
			"version_id":  os.Getenv("GAE_VERSION"),
			"instance_id": os.Getenv("GAE_INSTANCE"),
			"runtime":     os.Getenv("GAE_RUNTIME"),
			"zone":        zone,
		},
	}
}

func isCloudFunction() bool {
	// Reserved envvars in older function runtimes, e.g. Node.js 8, Python 3.7 and Go 1.11.
	_, name := os.LookupEnv("FUNCTION_NAME")
	_, region := os.LookupEnv("FUNCTION_REGION")
	_, entry := os.LookupEnv("ENTRY_POINT")

	// Reserved envvars in newer function runtimes.
	_, target := os.LookupEnv("FUNCTION_TARGET")
	_, signature := os.LookupEnv("FUNCTION_SIGNATURE_TYPE")
	_, service := os.LookupEnv("K_SERVICE")
	return (name && region && entry) || (target && signature && service)
}

func detectCloudFunction() *mrpb.MonitoredResource {
	projectID, err := metadata.ProjectID()
	if err != nil {
		return nil
	}
	zone, err := metadata.Zone()
	if err != nil {
		return nil
	}
	// Newer functions runtimes store name in K_SERVICE.
	functionName, exists := os.LookupEnv("K_SERVICE")
	if !exists {
		functionName, _ = os.LookupEnv("FUNCTION_NAME")
	}
	return &mrpb.MonitoredResource{
		Type: "cloud_function",
		Labels: map[string]string{
			"project_id":    projectID,
			"region":        regionFromZone(zone),
			"function_name": functionName,
		},
	}
}

func isCloudRun() bool {
	_, config := os.LookupEnv("K_CONFIGURATION")
	_, service := os.LookupEnv("K_SERVICE")
	_, revision := os.LookupEnv("K_REVISION")
	return config && service && revision
}

func detectCloudRunResource() *mrpb.MonitoredResource {
	projectID, err := metadata.ProjectID()
	if err != nil {
		return nil
	}
	zone, err := metadata.Zone()
	if err != nil {
		return nil
	}
	return &mrpb.MonitoredResource{
		Type: "cloud_run_revision",
		Labels: map[string]string{
			"project_id":         projectID,
			"location":           regionFromZone(zone),
			"service_name":       os.Getenv("K_SERVICE"),
			"revision_name":      os.Getenv("K_REVISION"),
			"configuration_name": os.Getenv("K_CONFIGURATION"),
		},
	}
}

func isKubernetesEngine() bool {
	clusterName, err := metadata.InstanceAttributeValue("cluster-name")
	// Note: InstanceAttributeValue can return "", nil
	if err != nil || clusterName == "" {
		return false
	}
	return true
}

func detectKubernetesResource() *mrpb.MonitoredResource {
	projectID, err := metadata.ProjectID()
	if err != nil {
		return nil
	}
	zone, err := metadata.Zone()
	if err != nil {
		return nil
	}
	clusterName, err := metadata.InstanceAttributeValue("cluster-name")
	if err != nil {
		return nil
	}
	namespaceBytes, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	namespaceName := ""
	if err == nil {
		namespaceName = string(namespaceBytes)
	}
	return &mrpb.MonitoredResource{
		Type: "k8s_container",
		Labels: map[string]string{
			"cluster_name":   clusterName,
			"location":       zone,
			"project_id":     projectID,
			"pod_name":       os.Getenv("HOSTNAME"),
			"namespace_name": namespaceName,
			// To get the `container_name` label, users need to explicitly provide it.
			"container_name": os.Getenv("CONTAINER_NAME"),
		},
	}
}

func detectGCEResource() *mrpb.MonitoredResource {
	projectID, err := metadata.ProjectID()
	if err != nil {
		return nil
	}
	id, err := metadata.InstanceID()
	if err != nil {
		return nil
	}
	zone, err := metadata.Zone()
	if err != nil {
		return nil
	}
	name, err := metadata.InstanceName()
	if err != nil {
		return nil
	}
	return &mrpb.MonitoredResource{
		Type: "gce_instance",
		Labels: map[string]string{
			"project_id":    projectID,
			"instance_id":   id,
			"instance_name": name,
			"zone":          zone,
		},
	}
}

func detectResource() *mrpb.MonitoredResource {
	detectedResource.once.Do(func() {
		switch {
		// AppEngine, Functions, CloudRun, Kubernetes are detected first,
		// as metadata.OnGCE() erroneously returns true on these runtimes.
		case isAppEngine():
			detectedResource.pb = detectAppEngineResource()
		case isCloudFunction():
			detectedResource.pb = detectCloudFunction()
		case isCloudRun():
			detectedResource.pb = detectCloudRunResource()
		case isKubernetesEngine():
			detectedResource.pb = detectKubernetesResource()
		case metadata.OnGCE():
			detectedResource.pb = detectGCEResource()
		}
	})
	return detectedResource.pb
}

var resourceInfo = map[string]struct{ rtype, label string }{
	"organizations":   {"organization", "organization_id"},
	"folders":         {"folder", "folder_id"},
	"projects":        {"project", "project_id"},
	"billingAccounts": {"billing_account", "account_id"},
}

func monitoredResource(parent string) *mrpb.MonitoredResource {
	parts := strings.SplitN(parent, "/", 2)
	if len(parts) != 2 {
		return globalResource(parent)
	}
	info, ok := resourceInfo[parts[0]]
	if !ok {
		return globalResource(parts[1])
	}
	return &mrpb.MonitoredResource{
		Type:   info.rtype,
		Labels: map[string]string{info.label: parts[1]},
	}
}

func regionFromZone(zone string) string {
	cutoff := strings.LastIndex(zone, "-")
	if cutoff > 0 {
		return zone[:cutoff]
	}
	return zone
}

func globalResource(projectID string) *mrpb.MonitoredResource {
	return &mrpb.MonitoredResource{
		Type: "global",
		Labels: map[string]string{
			"project_id": projectID,
		},
	}
}
