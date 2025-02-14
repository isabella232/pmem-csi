/*
Copyright 2020 Intel Corporation.

SPDX-License-Identifier: Apache-2.0
*/

package deployments

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/intel/pmem-csi/deploy"
	api "github.com/intel/pmem-csi/pkg/apis/pmemcsi/v1beta1"
	"github.com/intel/pmem-csi/pkg/types"
	"github.com/intel/pmem-csi/pkg/version"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
)

// LoadObjects reads all objects stored in a pmem-csi.yaml reference file.
func LoadObjects(kubernetes version.Version, deviceMode api.DeviceMode) ([]unstructured.Unstructured, error) {
	return loadYAML(yamlPath(kubernetes, deviceMode), nil, nil, nil)
}

var pmemImage = regexp.MustCompile(`image: intel/pmem-csi-driver(-test)?:\S+`)
var nameRegex = regexp.MustCompile(`(name|secretName|serviceName|serviceAccountName): pmem-csi-intel-com`)
var labelRegex = regexp.MustCompile(`pmem-csi.intel.com/(convert-raw-namespace)`)
var driverNameRegex = regexp.MustCompile(`(?m)(name|app\.kubernetes.io/instance): pmem-csi.intel.com$`)

// LoadAndCustomizeObjects reads all objects stored in a pmem-csi.yaml reference file
// and updates them on-the-fly according to the deployment spec, namespace and name.
func LoadAndCustomizeObjects(kubernetes version.Version, deviceMode api.DeviceMode,
	namespace string, deployment api.PmemCSIDeployment,
	controllerCABundle []byte,
) ([]unstructured.Unstructured, error) {

	// Conceptually this function is similar to calling "kustomize" for
	// our deployments. But because we controll the input, we can do some
	// things like renaming with a simple text search/replace.
	patchYAML := func(yaml *[]byte) {
		// This renames the objects and labels. A hyphen is used instead of a dot,
		// except for CSIDriver and instance label which need the exact name.
		*yaml = nameRegex.ReplaceAll(*yaml, []byte("$1: "+deployment.GetHyphenedName()))
		*yaml = labelRegex.ReplaceAll(*yaml, []byte(deployment.Name+"/$1"))
		*yaml = driverNameRegex.ReplaceAll(*yaml, []byte("$1: "+deployment.Name))

		// Update the driver name inside the state and socket dir.
		*yaml = bytes.ReplaceAll(*yaml, []byte("path: /var/lib/pmem-csi.intel.com"), []byte("path: /var/lib/"+deployment.Name))
		*yaml = bytes.ReplaceAll(*yaml, []byte("mountPath: /var/lib/pmem-csi.intel.com"), []byte("mountPath: /var/lib/"+deployment.Name))
		*yaml = bytes.ReplaceAll(*yaml, []byte("path: /var/lib/kubelet/plugins/pmem-csi.intel.com"), []byte("path: /var/lib/kubelet/plugins/"+deployment.Name))

		// Update kubelet path
		if deployment.Spec.KubeletDir != api.DefaultKubeletDir {
			*yaml = bytes.ReplaceAll(*yaml, []byte("/var/lib/kubelet"), []byte(deployment.Spec.KubeletDir))
		}

		// This assumes that all namespaced objects actually have "namespace: pmem-csi".
		*yaml = bytes.ReplaceAll(*yaml, []byte("namespace: pmem-csi"), []byte("namespace: "+namespace))

		// Also rename the prefix inside the registry endpoint.
		*yaml = bytes.ReplaceAll(*yaml,
			[]byte("tcp://pmem-csi"),
			[]byte("tcp://"+deployment.GetHyphenedName()))

		*yaml = bytes.ReplaceAll(*yaml,
			[]byte("imagePullPolicy: IfNotPresent"),
			[]byte("imagePullPolicy: "+deployment.Spec.PullPolicy))

		*yaml = bytes.ReplaceAll(*yaml,
			[]byte("-v=3"),
			[]byte(fmt.Sprintf("-v=%d", deployment.Spec.LogLevel)))

		if deployment.Spec.LogFormat != "" {
			*yaml = bytes.ReplaceAll(*yaml,
				[]byte("-logging-format=text"),
				[]byte(fmt.Sprintf("-logging-format=%s", deployment.Spec.LogFormat)))
		}

		nodeSelector := types.NodeSelector(deployment.Spec.NodeSelector)
		*yaml = bytes.ReplaceAll(*yaml,
			[]byte(`-nodeSelector={"storage":"pmem"}`),
			[]byte("-nodeSelector="+nodeSelector.String()))

		*yaml = pmemImage.ReplaceAll(*yaml, []byte("image: "+deployment.Spec.Image))
	}

	enabled := func(obj *unstructured.Unstructured) bool {
		return true
	}

	patchUnstructured := func(obj *unstructured.Unstructured) {
		if deployment.Spec.Labels != nil {
			labels := obj.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			for key, value := range deployment.Spec.Labels {
				labels[key] = value
			}
			obj.SetLabels(labels)
		}

		switch obj.GetKind() {
		case "Deployment":
			resources := map[string]*corev1.ResourceRequirements{
				"pmem-driver": deployment.Spec.ControllerDriverResources,
			}
			if err := patchPodTemplate(obj, deployment, resources); err != nil {
				// TODO: avoid panic
				panic(fmt.Errorf("set controller resources: %v", err))
			}
			outerSpec := obj.Object["spec"].(map[string]interface{})
			replicas := int64(deployment.Spec.ControllerReplicas)
			if replicas == 0 {
				replicas = 1
			}
			outerSpec["replicas"] = replicas
		case "DaemonSet":
			switch obj.GetName() {
			case deployment.NodeSetupName():
				if err := patchPodTemplate(obj, deployment, nil); err != nil {
					// TODO: avoid panic
					panic(fmt.Errorf("set node resources: %v", err))
				}
			case deployment.NodeDriverName():
				resources := map[string]*corev1.ResourceRequirements{
					"pmem-driver":          deployment.Spec.NodeDriverResources,
					"external-provisioner": deployment.Spec.ProvisionerResources,
					"driver-registrar":     deployment.Spec.NodeRegistrarResources,
				}
				if err := patchPodTemplate(obj, deployment, resources); err != nil {
					// TODO: avoid panic
					panic(fmt.Errorf("set node resources: %v", err))
				}
				outerSpec := obj.Object["spec"].(map[string]interface{})
				updateStrategy := outerSpec["updateStrategy"].(map[string]interface{})
				rollingUpdate := updateStrategy["rollingUpdate"].(map[string]interface{})
				rollingUpdate["maxUnavailable"] = deployment.Spec.MaxUnavailable
				template := outerSpec["template"].(map[string]interface{})
				spec := template["spec"].(map[string]interface{})
				if deployment.Spec.NodeSelector != nil {
					selector := map[string]interface{}{}
					for key, value := range deployment.Spec.NodeSelector {
						selector[key] = value
					}
					spec["nodeSelector"] = selector
				}
			}
		case "MutatingWebhookConfiguration":
			webhooks := obj.Object["webhooks"].([]interface{})
			failurePolicy := "Ignore"
			if deployment.Spec.MutatePods == api.MutatePodsAlways {
				failurePolicy = "Fail"
			}
			webhook := webhooks[0].(map[string]interface{})
			webhook["failurePolicy"] = failurePolicy
			clientConfig := webhook["clientConfig"].(map[string]interface{})
			if controllerCABundle != nil {
				clientConfig["caBundle"] = base64.StdEncoding.EncodeToString(controllerCABundle)

			}
			if deployment.Spec.ControllerTLSSecret == api.ControllerTLSSecretOpenshift {
				meta := obj.Object["metadata"].(map[string]interface{})
				meta["annotations"] = map[string]string{
					"service.beta.openshift.io/inject-cabundle": "true",
				}
			}
		case "Service":
			switch obj.GetName() {
			case deployment.SchedulerServiceName():
				spec := obj.Object["spec"].(map[string]interface{})
				ports := spec["ports"].([]interface{})
				port0 := ports[0].(map[string]interface{})
				if deployment.Spec.SchedulerNodePort != 0 {
					spec["type"] = "NodePort"
					port0["nodePort"] = deployment.Spec.SchedulerNodePort
				}
				if deployment.Spec.ControllerTLSSecret == api.ControllerTLSSecretOpenshift {
					port0["targetPort"] = 8001
					port0["port"] = 80
				}
			case deployment.WebhooksServiceName():
				if deployment.Spec.ControllerTLSSecret == api.ControllerTLSSecretOpenshift {
					meta := obj.Object["metadata"].(map[string]interface{})
					meta["annotations"] = map[string]string{
						"service.beta.openshift.io/serving-cert-secret-name": deployment.ControllerTLSSecretOpenshiftName(),
					}
				}
			}
		}
	}

	objects, err := loadYAML(yamlPath(kubernetes, deviceMode), patchYAML, enabled, patchUnstructured)
	if err != nil {
		return nil, err
	}

	scheduler, err := loadYAML("kustomize/scheduler/scheduler-service.yaml", patchYAML, enabled, patchUnstructured)
	if err != nil {
		return nil, err
	}
	objects = append(objects, scheduler...)

	if deployment.Spec.ControllerTLSSecret != "" && deployment.Spec.MutatePods != api.MutatePodsNever {
		webhook, err := loadYAML("kustomize/webhook/webhook.yaml", patchYAML, enabled, patchUnstructured)
		if err != nil {
			return nil, err
		}
		objects = append(objects, webhook...)
		service, err := loadYAML("kustomize/webhook/webhook-service.yaml", patchYAML, enabled, patchUnstructured)
		if err != nil {
			return nil, err
		}
		objects = append(objects, service...)
	}

	return objects, nil
}

func patchPodTemplate(obj *unstructured.Unstructured, deployment api.PmemCSIDeployment, resources map[string]*corev1.ResourceRequirements) error {
	outerSpec := obj.Object["spec"].(map[string]interface{})
	template := outerSpec["template"].(map[string]interface{})
	spec := template["spec"].(map[string]interface{})
	metadata := template["metadata"].(map[string]interface{})

	isController := strings.Contains(obj.GetName(), "controller")
	stripTLS := isController && deployment.Spec.ControllerTLSSecret == ""
	openshiftTLS := isController && deployment.Spec.ControllerTLSSecret == api.ControllerTLSSecretOpenshift

	if deployment.Spec.Labels != nil {
		labels := metadata["labels"]
		var labelsMap map[string]interface{}
		if labels == nil {
			labelsMap = map[string]interface{}{}
		} else {
			labelsMap = labels.(map[string]interface{})
		}
		for key, value := range deployment.Spec.Labels {
			labelsMap[key] = value
		}
		metadata["labels"] = labelsMap
	}

	if resources == nil {
		return nil
	}

	// Convert through JSON.
	resourceObj := func(r *corev1.ResourceRequirements) (map[string]interface{}, error) {
		obj := map[string]interface{}{}
		data, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &obj); err != nil {
			return nil, err
		}
		return obj, nil
	}

	if stripTLS {
		spec["volumes"] = nil
	}

	containers := spec["containers"].([]interface{})
	for _, container := range containers {
		container := container.(map[string]interface{})
		containerName := container["name"].(string)
		obj, err := resourceObj(resources[containerName])
		if err != nil {
			return err
		}
		container["resources"] = obj

		if stripTLS && container["name"].(string) == "pmem-driver" {
			container["volumeMounts"] = nil
			var command []interface{}
			for _, arg := range container["command"].([]interface{}) {
				switch strings.Split(arg.(string), "=")[0] {
				case "-caFile",
					"-certFile",
					"-keyFile",
					"-schedulerListen":
					// remove these parameters
				default:
					command = append(command, arg)
				}
			}
			container["command"] = command
		}

		if openshiftTLS && container["name"].(string) == "pmem-driver" {
			var command []interface{}
			for _, arg := range container["command"].([]interface{}) {
				switch arg.(string) {
				case "-schedulerListen=:8000":
					command = append(command, arg, "-insecureSchedulerListen=:8001")
				default:
					command = append(command, arg)
				}
			}
			container["command"] = command
		}

		// Override driver name in env var.
		env := container["env"]
		if env != nil {
			env := env.([]interface{})
			for _, entry := range env {
				entry := entry.(map[string]interface{})
				if entry["name"].(string) == "PMEM_CSI_DRIVER_NAME" {
					entry["value"] = deployment.GetName()
					break
				}
			}
		}

		var image string
		switch containerName {
		case "external-provisioner":
			image = deployment.Spec.ProvisionerImage
		case "driver-registrar":
			image = deployment.Spec.NodeRegistrarImage
		case "pmem-driver":
			cmd := container["command"].([]interface{})
			for i := range cmd {
				arg := cmd[i].(string)
				if strings.HasPrefix(arg, "-pmemPercentage=") {
					cmd[i] = fmt.Sprintf("-pmemPercentage=%d", deployment.Spec.PMEMPercentage)
					break
				}
			}
		}
		if image != "" {
			container["image"] = image
		}
	}

	if deployment.Spec.ControllerTLSSecret != "" {
		volumes := spec["volumes"].([]interface{})
		for _, volume := range volumes {
			volume := volume.(map[string]interface{})
			volumeName := volume["name"].(string)
			if volumeName == "webhook-cert" {
				name := deployment.Spec.ControllerTLSSecret
				if name == api.ControllerTLSSecretOpenshift {
					name = deployment.ControllerTLSSecretOpenshiftName()
				}
				volume["secret"].(map[string]interface{})["secretName"] = name
			}
		}
	}
	return nil
}

func yamlPath(kubernetes version.Version, deviceMode api.DeviceMode) string {
	return fmt.Sprintf("kubernetes-%s/pmem-csi-%s.yaml", kubernetes, deviceMode)
}

func loadYAML(path string,
	patchYAML func(yaml *[]byte),
	enabled func(obj *unstructured.Unstructured) bool,
	patchUnstructured func(obj *unstructured.Unstructured)) ([]unstructured.Unstructured, error) {
	// We load the builtin yaml files. If they exist, we prefer
	// the version without the patched in coverage support.
	yaml, err := deploy.Asset("nocoverage/" + path)
	if err != nil {
		yaml, err = deploy.Asset(path)
		if err != nil {
			return nil, fmt.Errorf("read reference yaml file: %w", err)
		}
	}

	// Split at the "---" separator before working on individual
	// item. Only works for .yaml.
	//
	// We need to split ourselves because we need access to each
	// original chunk of data for decoding. kubectl has its own
	// infrastructure for this, but that is a lot of code with
	// many dependencies.
	items := bytes.Split(yaml, []byte("\n---"))
	deserializer := scheme.Codecs.UniversalDeserializer()
	var objects []unstructured.Unstructured
	for _, item := range items {
		obj := unstructured.Unstructured{}
		if patchYAML != nil {
			patchYAML(&item)
		}
		_, _, err := deserializer.Decode(item, nil, &obj)
		if err != nil {
			return nil, fmt.Errorf("decode item %q from file %q: %v", item, path, err)
		}
		if enabled != nil && !enabled(&obj) {
			continue
		}
		if patchUnstructured != nil {
			patchUnstructured(&obj)
		}
		objects = append(objects, obj)
	}
	return objects, nil
}
