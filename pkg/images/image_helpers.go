/*
Copyright 2018 The kube-fledged authors.

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

package images

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/golang/glog"
	fledgedv1alpha3 "github.com/lcouds/kube-fledged/pkg/apis/kubefledged/v1alpha3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// newImagePullJob constructs a job manifest for pulling an image to a node
func newImagePullJob(imagecache *fledgedv1alpha3.ImageCache, image string,
	forceFullCache bool, node *corev1.Node, imagePullPolicy string,
	busyboxImage string, serviceAccountName string, jobPriorityClassName string) (*batchv1.Job, error) {
	var pullPolicy corev1.PullPolicy = corev1.PullIfNotPresent
	hostname := node.Labels["kubernetes.io/hostname"]
	if imagecache == nil {
		glog.Error("imagecache pointer is nil")
		return nil, fmt.Errorf("imagecache pointer is nil")
	}
	if imagePullPolicy == string(corev1.PullAlways) {
		pullPolicy = corev1.PullAlways
	} else if imagePullPolicy == string(corev1.PullIfNotPresent) {
		pullPolicy = corev1.PullIfNotPresent
		if latestimage := strings.Contains(image, ":latest") || !strings.Contains(image, ":"); latestimage {
			pullPolicy = corev1.PullAlways
		}
	}

	labels := map[string]string{
		"app":         "kubefledged",
		"kubefledged": "kubefledged-image-manager",
		"imagecache":  imagecache.Name,
		"controller":  controllerAgentName,
	}

	var job *batchv1.Job
	if forceFullCache {
		job = fullCacheJob(imagecache, image, pullPolicy, hostname, labels)
	} else if strings.Contains(image, "modelzai") {
		job = dirCacheJob(imagecache, image, pullPolicy, hostname, labels, []string{
			"/opt/conda/bin/", "/opt/conda/lib/",
		})
	} else {
		job = commonJob(imagecache, image, pullPolicy, hostname, labels, busyboxImage)
	}

	if serviceAccountName != "" {
		job.Spec.Template.Spec.ServiceAccountName = serviceAccountName
	}
	if jobPriorityClassName != "" {
		job.Spec.Template.Spec.PriorityClassName = jobPriorityClassName
	}
	return job, nil
}

// newImageDeleteJob constructs a job manifest to delete an image from a node
func newImageDeleteJob(imagecache *fledgedv1alpha3.ImageCache, image string, node *corev1.Node,
	containerRuntimeVersion string, dockerclientimage string, serviceAccountName string,
	imageDeleteJobHostNetwork bool, jobPriorityClassName string, criSocketPath string) (*batchv1.Job, error) {
	hostname := node.Labels["kubernetes.io/hostname"]
	socketPath := criSocketPath
	if imagecache == nil {
		glog.Error("imagecache pointer is nil")
		return nil, fmt.Errorf("imagecache pointer is nil")
	}

	labels := map[string]string{
		"app":         "kubefledged",
		"kubefledged": "kubefledged-image-manager",
		"imagecache":  imagecache.Name,
		"controller":  controllerAgentName,
	}

	hostpathtype := corev1.HostPathSocket
	backoffLimit := int32(0)
	activeDeadlineSeconds := int64((time.Hour).Seconds())

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: imagecache.Name + "-",
			Namespace:    imagecache.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(imagecache, schema.GroupVersionKind{
					Group:   fledgedv1alpha3.SchemeGroupVersion.Group,
					Version: fledgedv1alpha3.SchemeGroupVersion.Version,
					Kind:    "ImageCache",
				}),
			},
			Labels: labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:          &backoffLimit,
			ActiveDeadlineSeconds: &activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: imagecache.Namespace,
					Labels:    labels,
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						"kubernetes.io/hostname": hostname,
					},
					Containers: []corev1.Container{
						{
							Name:    "docker-cri-client",
							Image:   dockerclientimage,
							Command: []string{"/bin/bash"},
							Args:    []string{"-c", "exec /usr/bin/docker image rm -f " + image + " > /dev/termination-log 2>&1"},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "runtime-sock",
									MountPath: "/var/run/docker.sock",
								},
							},
							ImagePullPolicy: corev1.PullIfNotPresent,
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "runtime-sock",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/run/docker.sock",
									Type: &hostpathtype,
								},
							},
						},
					},
					RestartPolicy:    corev1.RestartPolicyNever,
					ImagePullSecrets: imagecache.Spec.ImagePullSecrets,
					HostNetwork:      imageDeleteJobHostNetwork,
					Tolerations: []corev1.Toleration{
						{
							Operator: corev1.TolerationOpExists,
						},
					},
				},
			},
		},
	}
	if strings.Contains(containerRuntimeVersion, "containerd") {
		if criSocketPath == "" {
			socketPath = "/run/containerd/containerd.sock"
		}
		deleteCommand := "exec /usr/bin/crictl --runtime-endpoint=unix://" + socketPath + " --image-endpoint=unix://" + socketPath + " rmi " + image + " > /dev/termination-log 2>&1"
		job.Spec.Template.Spec.Containers[0].Args = []string{"-c", deleteCommand}
		job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = socketPath
		job.Spec.Template.Spec.Volumes[0].VolumeSource.HostPath.Path = socketPath
	}
	if strings.Contains(containerRuntimeVersion, "crio") || strings.Contains(containerRuntimeVersion, "cri-o") {
		if criSocketPath == "" {
			socketPath = "/var/run/crio/crio.sock"
		}
		deleteCommand := "exec /usr/bin/crictl --runtime-endpoint=unix://" + socketPath + " --image-endpoint=unix://" + socketPath + " rmi " + image + " > /dev/termination-log 2>&1"
		job.Spec.Template.Spec.Containers[0].Args = []string{"-c", deleteCommand}
		job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = socketPath
		job.Spec.Template.Spec.Volumes[0].VolumeSource.HostPath.Path = socketPath
	}
	if strings.Contains(containerRuntimeVersion, "docker") {
		if criSocketPath == "" {
			socketPath = "/var/run/docker.sock"
		}
		job.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath = socketPath
		job.Spec.Template.Spec.Volumes[0].VolumeSource.HostPath.Path = socketPath
	}
	if serviceAccountName != "" {
		job.Spec.Template.Spec.ServiceAccountName = serviceAccountName
	}
	if jobPriorityClassName != "" {
		job.Spec.Template.Spec.PriorityClassName = jobPriorityClassName
	}
	return job, nil
}

func checkIfImageNeedsToBePulled(imagePullPolicy string, image string, node *corev1.Node) (bool, error) {
	if imagePullPolicy == string(corev1.PullIfNotPresent) {
		if !strings.Contains(image, ":") && !strings.Contains(image, "@sha") {
			return true, nil
		}
		if strings.Contains(image, ":latest") {
			return true, nil
		}
		imageAlreadyPresent, err := imageAlreadyPresentInNode(image, node)
		if err != nil {
			return false, err
		}
		if imageAlreadyPresent {
			return false, nil
		}
	}
	return true, nil
}

func imageAlreadyPresentInNode(image string, node *corev1.Node) (bool, error) {
	imagesByteSlice, err := json.Marshal(node.Status.Images)
	if err != nil {
		return false, err
	}
	if strings.Contains(string(imagesByteSlice), image) {
		return true, nil
	}
	return false, nil
}
