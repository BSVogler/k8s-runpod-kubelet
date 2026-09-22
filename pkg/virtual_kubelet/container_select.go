package runpod

import (
	"strings"

	v1 "k8s.io/api/core/v1"
)

// DefaultContainerAnnotation is the kubectl/K8s hint for the primary container.
const DefaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

const mainContainerName = "main"

var sidecarNameExact = map[string]struct{}{
	"web-terminal": {},
	"filebeat":     {},
	"istio-proxy":  {},
	"linkerd-proxy": {},
	"docker-proxy": {},
	"dcgm-exporter": {},
}

var sidecarNameContains = []string{
	"cosfs",
	"save-image",
	"web-terminal",
	"docker-proxy",
	"filebeat",
	"sidecar",
	"dcgm",
}

var sidecarImageContains = []string{
	"cosfs",
	"web-terminal",
	"save-image",
	"docker-proxy",
	"filebeat",
	"dcgm-exporter",
	"istio/proxy",
}

// SelectWorkloadContainer picks the container whose image/env/command go to RunPod.
// Sidecars are skipped; among the rest prefer name "main", then
// kubectl.kubernetes.io/default-container, then the first remaining.
// If every container is a sidecar, fall back to containers[0].
func SelectWorkloadContainer(pod *v1.Pod) *v1.Container {
	idx := selectWorkloadContainerIndex(pod)
	if idx < 0 {
		return nil
	}
	return &pod.Spec.Containers[idx]
}

func selectWorkloadContainerIndex(pod *v1.Pod) int {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return -1
	}

	var work []int
	for i, c := range pod.Spec.Containers {
		if !isSidecarContainer(c) {
			work = append(work, i)
		}
	}
	if len(work) == 0 {
		return 0
	}

	for _, i := range work {
		if strings.EqualFold(pod.Spec.Containers[i].Name, mainContainerName) {
			return i
		}
	}

	if pod.Annotations != nil {
		if want := strings.TrimSpace(pod.Annotations[DefaultContainerAnnotation]); want != "" {
			for _, i := range work {
				if pod.Spec.Containers[i].Name == want {
					return i
				}
			}
		}
	}

	return work[0]
}

func isSidecarContainer(c v1.Container) bool {
	if strings.EqualFold(c.Name, mainContainerName) {
		return false
	}
	name := strings.ToLower(c.Name)
	if _, ok := sidecarNameExact[name]; ok {
		return true
	}
	for _, p := range sidecarNameContains {
		if strings.Contains(name, p) {
			return true
		}
	}
	image := strings.ToLower(c.Image)
	for _, p := range sidecarImageContains {
		if strings.Contains(image, p) {
			return true
		}
	}
	return false
}
