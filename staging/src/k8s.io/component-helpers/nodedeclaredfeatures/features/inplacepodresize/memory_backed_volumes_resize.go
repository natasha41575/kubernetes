/*
Copyright 2026 The Kubernetes Authors.

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

package inplacepodresize

import (
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-helpers/nodedeclaredfeatures/types"
)

// Ensure the feature struct implements the unified Feature interface.
var _ types.Feature = &memoryBackedVolumesResizeFeature{}

// IPPRMemoryBackedVolumesFeatureGate is the feature gate for InPlacePodVerticalScalingMemoryBackedVolumes.
const IPPRMemoryBackedVolumesFeatureGate = "InPlacePodVerticalScalingMemoryBackedVolumes"

// MemoryBackedVolumesResizeFeature is the implementation of the `InPlacePodVerticalScalingMemoryBackedVolumes` feature.
var MemoryBackedVolumesResizeFeature = &memoryBackedVolumesResizeFeature{}

type memoryBackedVolumesResizeFeature struct{}

func (f *memoryBackedVolumesResizeFeature) Name() string {
	return IPPRMemoryBackedVolumesFeatureGate
}

func (f *memoryBackedVolumesResizeFeature) Discover(cfg *types.NodeConfiguration) bool {
	return cfg.FeatureGates.Enabled(IPPRMemoryBackedVolumesFeatureGate)
}

func (f *memoryBackedVolumesResizeFeature) Requirements() *types.FeatureRequirements {
	return &types.FeatureRequirements{
		EnabledFeatureGates: []string{IPPRMemoryBackedVolumesFeatureGate},
	}
}

func (f *memoryBackedVolumesResizeFeature) InferForScheduling(podInfo *types.PodInfo) bool {
	// This feature is only relevant for pod updates.
	return false
}

func (f *memoryBackedVolumesResizeFeature) InferForUpdate(oldPodInfo, newPodInfo *types.PodInfo) bool {
	oldVolumes := make(map[string]*v1.Volume)
	for i := range oldPodInfo.Spec.Volumes {
		vol := &oldPodInfo.Spec.Volumes[i]
		oldVolumes[vol.Name] = vol
	}

	for i := range newPodInfo.Spec.Volumes {
		newVol := &newPodInfo.Spec.Volumes[i]
		if isMemoryBackedEmptyDir(newVol) {
			oldVol, exists := oldVolumes[newVol.Name]
			if !exists || !isMemoryBackedEmptyDir(oldVol) {
				return true
			}
			if !quantityEqual(newVol.EmptyDir.SizeLimit, oldVol.EmptyDir.SizeLimit) {
				return true
			}
		}
	}

	for _, oldVol := range oldVolumes {
		if isMemoryBackedEmptyDir(oldVol) {
			found := false
			for j := range newPodInfo.Spec.Volumes {
				if newPodInfo.Spec.Volumes[j].Name == oldVol.Name {
					found = true
					break
				}
			}
			if !found {
				return true
			}
		}
	}

	return false
}

func (f *memoryBackedVolumesResizeFeature) MaxVersion() *version.Version {
	return nil
}

func isMemoryBackedEmptyDir(vol *v1.Volume) bool {
	return vol.EmptyDir != nil && vol.EmptyDir.Medium == v1.StorageMediumMemory
}

func quantityEqual(q1, q2 *resource.Quantity) bool {
	if q1 == nil && q2 == nil {
		return true
	}
	if q1 == nil || q2 == nil {
		return false
	}
	return q1.Cmp(*q2) == 0
}
