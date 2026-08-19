/*
Copyright The Kubernetes Authors.

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

package state

import (
	"encoding/json"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/checksum"
)

// PodResourceInfoV1 is the legacy checkpoint structure used in v1.37 and earlier.
type PodResourceInfoV1 struct {
	ContainerResources   map[string]v1.ResourceRequirements `json:"containerResources,omitempty"`
	PodLevelResources    *v1.ResourceRequirements           `json:"podLevelResources,omitempty"`
	EmptyDirVolumeLimits map[string]*resource.Quantity      `json:"emptyDirVolumeLimits,omitempty"`
}

type PodResourceCheckpointInfoV1 struct {
	Entries map[types.UID]PodResourceInfoV1 `json:"entries,omitempty"`
}

type checkpointJSONV1 struct {
	Data     string            `json:"data"`
	Checksum checksum.Checksum `json:"checksum"`
}

var _ checkpointmanager.Checkpoint = &checkpointJSONV1{}

func (cp *checkpointJSONV1) MarshalCheckpoint() ([]byte, error) {
	return json.Marshal(cp)
}

func (cp *checkpointJSONV1) UnmarshalCheckpoint(blob []byte) error {
	return json.Unmarshal(blob, cp)
}

func (cp *checkpointJSONV1) VerifyChecksum() error {
	return cp.Checksum.Verify(cp.Data)
}

func migrateV1ToPodList(data string) (*v1.PodList, error) {
	var checkpointData PodResourceCheckpointInfoV1
	if err := json.Unmarshal([]byte(data), &checkpointData); err != nil {
		return nil, err
	}

	podList := &v1.PodList{
		Items: make([]v1.Pod, 0, len(checkpointData.Entries)),
	}
	for podUID, entry := range checkpointData.Entries {
		pod := v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID: podUID,
			},
		}
		for containerName, resources := range entry.ContainerResources {
			pod.Spec.Containers = append(pod.Spec.Containers, v1.Container{
				Name:      containerName,
				Resources: *resources.DeepCopy(),
			})
		}
		if entry.PodLevelResources != nil {
			pod.Spec.Resources = entry.PodLevelResources.DeepCopy()
		}
		for volName, limit := range entry.EmptyDirVolumeLimits {
			var limitCopy *resource.Quantity
			if limit != nil {
				lc := limit.DeepCopy()
				limitCopy = &lc
			}
			pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{
				Name: volName,
				VolumeSource: v1.VolumeSource{
					EmptyDir: &v1.EmptyDirVolumeSource{
						SizeLimit: limitCopy,
					},
				},
			})
		}
		podList.Items = append(podList.Items, pod)
	}
	return podList, nil
}
