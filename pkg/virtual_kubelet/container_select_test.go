package runpod

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSelectWorkloadContainer(t *testing.T) {
	t.Parallel()

	sidecar := v1.Container{Name: "cosfs", Image: "ccr.ccs.tencentyun.com/ti/cosfs:v1"}
	webterm := v1.Container{Name: "web-terminal", Image: "example.com/web-terminal:1"}
	app := v1.Container{Name: "notebook", Image: "tione/notebook:cuda13"}
	main := v1.Container{Name: "main", Image: "tione/vllm:latest"}

	tests := []struct {
		name      string
		pod       *v1.Pod
		wantName  string
		wantImage string
		wantNil   bool
	}{
		{
			name:    "empty",
			pod:     &v1.Pod{},
			wantNil: true,
		},
		{
			name: "sidecar first then app",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				sidecar, webterm, app,
			}}},
			wantName:  "notebook",
			wantImage: "tione/notebook:cuda13",
		},
		{
			name: "prefer main among non-sidecars",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				sidecar, app, main,
			}}},
			wantName:  "main",
			wantImage: "tione/vllm:latest",
		},
		{
			name: "annotation default-container",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						DefaultContainerAnnotation: "notebook",
					},
				},
				Spec: v1.PodSpec{Containers: []v1.Container{sidecar, app, main}},
			},
			// main name still wins over annotation
			wantName: "main",
		},
		{
			name: "annotation when no main name",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						DefaultContainerAnnotation: "notebook",
					},
				},
				Spec: v1.PodSpec{Containers: []v1.Container{
					sidecar,
					{Name: "helper", Image: "busybox"},
					app,
				}},
			},
			wantName: "notebook",
		},
		{
			name: "only sidecars fall back to first",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				sidecar, webterm,
			}}},
			wantName:  "cosfs",
			wantImage: "ccr.ccs.tencentyun.com/ti/cosfs:v1",
		},
		{
			name: "single container",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				app,
			}}},
			wantName: "notebook",
		},
		{
			name: "main never treated as sidecar",
			pod: &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{
				{Name: "main", Image: "example.com/cosfs-wrapper:1"},
			}}},
			wantName: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SelectWorkloadContainer(tt.pod)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %s", got.Name)
				}
				return
			}
			if got == nil {
				t.Fatal("want container, got nil")
			}
			if got.Name != tt.wantName {
				t.Fatalf("name: got %q want %q", got.Name, tt.wantName)
			}
			if tt.wantImage != "" && got.Image != tt.wantImage {
				t.Fatalf("image: got %q want %q", got.Image, tt.wantImage)
			}
		})
	}
}
