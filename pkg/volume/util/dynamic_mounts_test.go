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
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	volumetest "k8s.io/kubernetes/pkg/volume/testing"
	"k8s.io/mount-utils"
)

func TestDynamicMountsTrackingFile(t *testing.T) {
	dir := "/var/lib/kubelet/pods/123/volumes/kubernetes.io~empty-dir/myvol"
	expected := "/var/lib/kubelet/pods/123/volumes/kubernetes.io~empty-dir/.myvol.dynamic_mounts"
	actual := DynamicMountsTrackingFile(dir, "myvol")
	if actual != expected {
		t.Fatalf("expected %q, got %q", expected, actual)
	}
}

func TestFindParentHostVolumeDir(t *testing.T) {
	tmpDir := t.TempDir()
	pluginDir := filepath.Join(tmpDir, "kubernetes.io~empty-dir")
	parentVolDir := filepath.Join(pluginDir, "dynamic-root")
	if err := os.MkdirAll(parentVolDir, 0755); err != nil {
		t.Fatalf("failed to create test dirs: %v", err)
	}

	found := FindParentHostVolumeDir(tmpDir, "dynamic-root")
	if found != parentVolDir {
		t.Fatalf("expected to find %q, got %q", parentVolDir, found)
	}

	notFound := FindParentHostVolumeDir(tmpDir, "nonexistent")
	if notFound != "" {
		t.Fatalf("expected empty string for nonexistent volume, got %q", notFound)
	}
}

func TestSyncAndCleanupDynamicPropagationMounts(t *testing.T) {
	tmpDir := t.TempDir()
	fakeHost := volumetest.NewFakeKubeletVolumeHost(t, tmpDir, nil, nil)
	fakeMounter := &mount.FakeMounter{}

	// Create directory structure
	volumesRootDir := filepath.Join(tmpDir, "volumes")
	emptyDirPluginDir := filepath.Join(volumesRootDir, "kubernetes.io~empty-dir")
	parentHostDir := filepath.Join(emptyDirPluginDir, "dynamic-root")
	childHostDir := filepath.Join(emptyDirPluginDir, "dynamic-child")

	if err := os.MkdirAll(parentHostDir, 0777); err != nil {
		t.Fatalf("failed to create parent dir: %v", err)
	}
	if err := os.MkdirAll(childHostDir, 0777); err != nil {
		t.Fatalf("failed to create child dir: %v", err)
	}

	hostToContainer := v1.MountPropagationHostToContainer
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
			UID:  types.UID("test-uid"),
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name: "app",
					VolumeMounts: []v1.VolumeMount{
						{
							Name:             "dynamic-root",
							MountPath:        "/mnt/dynamic",
							MountPropagation: &hostToContainer,
						},
						{
							Name:      "dynamic-child",
							MountPath: "/mnt/dynamic/child",
						},
					},
				},
			},
		},
	}

	// 1. Sync dynamic propagation mounts (Hot-Plug)
	SyncDynamicPropagationMounts(fakeMounter, fakeHost, pod, "dynamic-child", childHostDir)

	trackingFile := DynamicMountsTrackingFile(childHostDir, "dynamic-child")
	data, err := os.ReadFile(trackingFile)
	if err != nil {
		t.Fatalf("expected tracking file to exist: %v", err)
	}

	expectedTarget := filepath.Join(parentHostDir, "child")
	if string(data) != expectedTarget+"\n" {
		t.Fatalf("expected tracking file content %q, got %q", expectedTarget+"\n", string(data))
	}

	// Verify FakeMounter received Mount call
	log := fakeMounter.GetLog()
	if len(log) == 0 {
		t.Fatalf("expected fakeMounter to record mount action, got none")
	}
	lastAction := log[len(log)-1]
	if lastAction.Action != mount.FakeActionMount || lastAction.Target != expectedTarget {
		t.Fatalf("unexpected mount action: %#v", lastAction)
	}

	// 2. Cleanup dynamic propagation mounts (Hot-Unplug)
	CleanupDynamicPropagationMounts(fakeMounter, "dynamic-child", childHostDir)

	if _, err := os.Stat(trackingFile); !os.IsNotExist(err) {
		t.Fatalf("expected tracking file to be deleted, stat err: %v", err)
	}
}
