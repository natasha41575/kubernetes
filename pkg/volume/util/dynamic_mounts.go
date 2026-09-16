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

package util

import (
	"os"
	"path/filepath"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/mount-utils"
)

// DynamicMountsTrackingFile returns the path to the tracking file for a dynamic volume mount.
func DynamicMountsTrackingFile(dir, volName string) string {
	return filepath.Join(filepath.Dir(dir), "."+volName+".dynamic_mounts")
}

// SyncDynamicPropagationMounts bind-mounts a volume into any parent host directory
// mounted with HostToContainer or Bidirectional propagation when a container mounts this volume
// as a subpath of that parent volume mount. This enables live hot-plug into running containers.
func SyncDynamicPropagationMounts(mounter mount.Interface, host volume.VolumeHost, pod *v1.Pod, volName, dir string) {
	if mounter == nil || host == nil || pod == nil {
		return
	}

	volumesRootDir := filepath.Dir(filepath.Dir(dir))
	var trackedPaths []string

	for _, c := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		var targetMounts []v1.VolumeMount
		var parentMounts []v1.VolumeMount

		for _, vm := range c.VolumeMounts {
			if vm.Name == volName {
				targetMounts = append(targetMounts, vm)
			} else if vm.MountPropagation != nil &&
				(*vm.MountPropagation == v1.MountPropagationHostToContainer || *vm.MountPropagation == v1.MountPropagationBidirectional) {
				parentMounts = append(parentMounts, vm)
			}
		}

		for _, targetMount := range targetMounts {
			for _, parentMount := range parentMounts {
				rel, err := filepath.Rel(parentMount.MountPath, targetMount.MountPath)
				if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
					continue
				}

				parentHostDir := FindParentHostVolumeDir(volumesRootDir, parentMount.Name)
				if parentHostDir == "" {
					klog.Warningf("Could not find host directory for parent volume %s (pod %s)", parentMount.Name, pod.UID)
					continue
				}

				if kletHost, ok := host.(volume.KubeletVolumeHost); ok && kletHost.GetHostUtil() != nil {
					_ = kletHost.GetHostUtil().MakeRShared(parentHostDir)
				}

				targetHostPath := filepath.Join(parentHostDir, rel)
				if err := os.MkdirAll(targetHostPath, 0777); err != nil {
					klog.Errorf("Failed to create target host directory %s for dynamic volume %s: %v", targetHostPath, volName, err)
					continue
				}

				notMnt, err := mounter.IsLikelyNotMountPoint(targetHostPath)
				if err == nil && notMnt {
					if mountErr := mounter.Mount(dir, targetHostPath, "", []string{"bind"}); mountErr != nil {
						klog.Errorf("Failed to bind-mount dynamic volume %s from %s to %s: %v", volName, dir, targetHostPath, mountErr)
						continue
					}
					klog.Infof("Dynamically hot-plugged volume %s from %s into %s (container path %s)", volName, dir, targetHostPath, targetMount.MountPath)
				}
				trackedPaths = append(trackedPaths, targetHostPath)
			}
		}
	}

	if len(trackedPaths) > 0 {
		trackingFile := DynamicMountsTrackingFile(dir, volName)
		_ = os.WriteFile(trackingFile, []byte(strings.Join(trackedPaths, "\n")+"\n"), 0600)
	}
}

// FindParentHostVolumeDir finds the host directory corresponding to parentVolName under volumesRootDir.
func FindParentHostVolumeDir(volumesRootDir, parentVolName string) string {
	entries, err := os.ReadDir(volumesRootDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(volumesRootDir, entry.Name(), parentVolName)
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// CleanupDynamicPropagationMounts unmounts and cleans up any dynamic bind mounts created
// under parent propagated volumes.
func CleanupDynamicPropagationMounts(mounter mount.Interface, volName, dir string) {
	if mounter == nil {
		return
	}
	trackingFile := DynamicMountsTrackingFile(dir, volName)
	data, err := os.ReadFile(trackingFile)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		targetHostPath := strings.TrimSpace(line)
		if targetHostPath == "" {
			continue
		}
		if err := mount.CleanupMountPoint(targetHostPath, mounter, true); err != nil {
			klog.Warningf("Failed to cleanup dynamic propagation mount %s for volume %s: %v", targetHostPath, volName, err)
		} else {
			klog.Infof("Dynamically hot-unplugged propagated mount %s for volume %s", targetHostPath, volName)
		}
	}
	_ = os.Remove(trackingFile)
}
