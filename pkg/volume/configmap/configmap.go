/*
Copyright 2015 The Kubernetes Authors.

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

package configmap

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilstrings "k8s.io/utils/strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/volume"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

// ProbeVolumePlugins is the entry point for plugin detection in a package.
func ProbeVolumePlugins() []volume.VolumePlugin {
	return []volume.VolumePlugin{&configMapPlugin{}}
}

const (
	configMapPluginName = "kubernetes.io/configmap"
)

// configMapPlugin implements the VolumePlugin interface.
type configMapPlugin struct {
	host         volume.VolumeHost
	getConfigMap func(namespace, name string) (*v1.ConfigMap, error)
}

var _ volume.VolumePlugin = &configMapPlugin{}

func getPath(uid types.UID, volName string, host volume.VolumeHost) string {
	return host.GetPodVolumeDir(uid, utilstrings.EscapeQualifiedName(configMapPluginName), volName)
}

func (plugin *configMapPlugin) Init(host volume.VolumeHost) error {
	plugin.host = host
	plugin.getConfigMap = host.GetConfigMapFunc()
	return nil
}

func (plugin *configMapPlugin) GetPluginName() string {
	return configMapPluginName
}

func (plugin *configMapPlugin) GetVolumeName(spec *volume.Spec) (string, error) {
	volumeSource, _ := getVolumeSource(spec)
	if volumeSource == nil {
		return "", fmt.Errorf("Spec does not reference a ConfigMap volume type")
	}

	return fmt.Sprintf(
		"%v/%v",
		spec.Name(),
		volumeSource.Name), nil
}

func (plugin *configMapPlugin) CanSupport(spec *volume.Spec) bool {
	return spec.Volume != nil && spec.Volume.ConfigMap != nil
}

func (plugin *configMapPlugin) RequiresRemount(spec *volume.Spec) bool {
	return true
}

func (plugin *configMapPlugin) SupportsMountOption() bool {
	return false
}

func (plugin *configMapPlugin) SupportsSELinuxContextMount(spec *volume.Spec) (bool, error) {
	return false, nil
}

func (plugin *configMapPlugin) NewMounter(spec *volume.Spec, pod *v1.Pod) (volume.Mounter, error) {
	return &configMapVolumeMounter{
		configMapVolume: &configMapVolume{
			spec.Name(),
			pod.UID,
			plugin,
			plugin.host.GetMounter(),
			volume.NewCachedMetrics(volume.NewMetricsDu(getPath(pod.UID, spec.Name(), plugin.host))),
		},
		source:       *spec.Volume.ConfigMap,
		pod:          *pod,
		getConfigMap: plugin.getConfigMap,
	}, nil
}

func (plugin *configMapPlugin) NewUnmounter(volName string, podUID types.UID) (volume.Unmounter, error) {
	return &configMapVolumeUnmounter{
		&configMapVolume{
			volName,
			podUID,
			plugin,
			plugin.host.GetMounter(),
			volume.NewCachedMetrics(volume.NewMetricsDu(getPath(podUID, volName, plugin.host))),
		},
	}, nil
}

func (plugin *configMapPlugin) ConstructVolumeSpec(volumeName, mountPath string) (volume.ReconstructedVolume, error) {
	configMapVolume := &v1.Volume{
		Name: volumeName,
		VolumeSource: v1.VolumeSource{
			ConfigMap: &v1.ConfigMapVolumeSource{},
		},
	}
	return volume.ReconstructedVolume{
		Spec: volume.NewSpecFromVolume(configMapVolume),
	}, nil
}

type configMapVolume struct {
	volName string
	podUID  types.UID
	plugin  *configMapPlugin
	mounter mount.Interface
	volume.MetricsProvider
}

var _ volume.Volume = &configMapVolume{}

func (sv *configMapVolume) GetPath() string {
	return sv.plugin.host.GetPodVolumeDir(sv.podUID, utilstrings.EscapeQualifiedName(configMapPluginName), sv.volName)
}

// configMapVolumeMounter handles retrieving secrets from the API server
// and placing them into the volume on the host.
type configMapVolumeMounter struct {
	*configMapVolume

	source       v1.ConfigMapVolumeSource
	pod          v1.Pod
	getConfigMap func(namespace, name string) (*v1.ConfigMap, error)
}

var _ volume.Mounter = &configMapVolumeMounter{}

func (sv *configMapVolume) GetAttributes() volume.Attributes {
	return volume.Attributes{
		ReadOnly:       true,
		Managed:        true,
		SELinuxRelabel: true,
	}
}

func wrappedVolumeSpec() volume.Spec {
	// This is the spec for the volume that this plugin wraps.
	return volume.Spec{
		// This should be on a tmpfs instead of the local disk; the problem is
		// charging the memory for the tmpfs to the right cgroup.  We should make
		// this a tmpfs when we can do the accounting correctly.
		Volume: &v1.Volume{VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
	}
}

func (b *configMapVolumeMounter) SetUp(mounterArgs volume.MounterArgs) error {
	return b.SetUpAt(b.GetPath(), mounterArgs)
}

func (b *configMapVolumeMounter) SetUpAt(dir string, mounterArgs volume.MounterArgs) error {
	klog.V(3).Infof("Setting up volume %v for pod %v at %v", b.volName, b.pod.UID, dir)

	// Wrap EmptyDir, let it do the setup.
	wrapped, err := b.plugin.host.NewWrapperMounter(b.volName, wrappedVolumeSpec(), &b.pod)
	if err != nil {
		return err
	}

	optional := b.source.Optional != nil && *b.source.Optional
	configMap, err := b.getConfigMap(b.pod.Namespace, b.source.Name)
	if err != nil {
		if !(errors.IsNotFound(err) && optional) {
			klog.Errorf("Couldn't get configMap %v/%v: %v", b.pod.Namespace, b.source.Name, err)
			return err
		}
		configMap = &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: b.pod.Namespace,
				Name:      b.source.Name,
			},
		}
	}

	totalBytes := totalBytes(configMap)
	klog.V(3).Infof("Received configMap %v/%v containing (%v) pieces of data, %v total bytes",
		b.pod.Namespace,
		b.source.Name,
		len(configMap.Data)+len(configMap.BinaryData),
		totalBytes)

	payload, err := MakePayload(b.source.Items, configMap, b.source.DefaultMode, b.source.DefaultUser, optional)
	if err != nil {
		return err
	}

	setupSuccess := false
	if err := wrapped.SetUpAt(dir, mounterArgs); err != nil {
		return err
	}
	if err := volumeutil.MakeNestedMountpoints(b.volName, dir, b.pod); err != nil {
		return err
	}

	defer func() {
		// Clean up directories if setup fails
		if !setupSuccess {
			unmounter, unmountCreateErr := b.plugin.NewUnmounter(b.volName, b.podUID)
			if unmountCreateErr != nil {
				klog.Errorf("error cleaning up mount %s after failure. Create unmounter failed with %v", b.volName, unmountCreateErr)
				return
			}
			tearDownErr := unmounter.TearDown()
			if tearDownErr != nil {
				klog.Errorf("error tearing down volume %s: %v", b.volName, tearDownErr)
			}
		}
	}()

	writerContext := fmt.Sprintf("pod %v/%v volume %v", b.pod.Namespace, b.pod.Name, b.volName)
	writer, err := volumeutil.NewAtomicWriter(dir, writerContext)
	if err != nil {
		klog.Errorf("Error creating atomic writer: %v", err)
		return err
	}

	setPerms := func(_ string) error {
		// This may be the first time writing and new files get created outside the timestamp subdirectory:
		// change the permissions on the whole volume and not only in the timestamp directory.
		ownerShipChanger := volume.NewVolumeOwnership(b, dir, mounterArgs.FsGroup, nil /*fsGroupChangePolicy*/, volumeutil.FSGroupCompleteHook(b.plugin, nil))
		return ownerShipChanger.ChangePermissions()
	}
	err = writer.Write(payload, setPerms)
	if err != nil {
		klog.Errorf("Error writing payload to dir: %v", err)
		return err
	}

	b.syncDynamicPropagationMounts(dir)

	setupSuccess = true
	return nil
}

func dynamicMountsTrackingFile(dir, volName string) string {
	return filepath.Join(filepath.Dir(dir), "."+volName+".dynamic_mounts")
}

// syncDynamicPropagationMounts bind-mounts this configmap volume into any parent host directory
// mounted with HostToContainer or Bidirectional propagation when a container mounts this volume
// as a subpath of that parent volume mount. This enables live hot-plug into running containers.
func (b *configMapVolumeMounter) syncDynamicPropagationMounts(dir string) {
	if b.mounter == nil || b.plugin == nil || b.plugin.host == nil {
		return
	}

	volumesRootDir := filepath.Dir(filepath.Dir(dir))
	var trackedPaths []string

	for _, c := range append(b.pod.Spec.Containers, b.pod.Spec.InitContainers...) {
		var targetMounts []v1.VolumeMount
		var parentMounts []v1.VolumeMount

		for _, vm := range c.VolumeMounts {
			if vm.Name == b.volName {
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

				parentHostDir := findParentHostVolumeDir(volumesRootDir, parentMount.Name)
				if parentHostDir == "" {
					klog.Warningf("Could not find host directory for parent volume %s (pod %s)", parentMount.Name, b.pod.UID)
					continue
				}

				if kletHost, ok := b.plugin.host.(volume.KubeletVolumeHost); ok && kletHost.GetHostUtil() != nil {
					_ = kletHost.GetHostUtil().MakeRShared(parentHostDir)
				}

				targetHostPath := filepath.Join(parentHostDir, rel)
				if err := os.MkdirAll(targetHostPath, 0755); err != nil {
					klog.Errorf("Failed to create target host directory %s for dynamic volume %s: %v", targetHostPath, b.volName, err)
					continue
				}

				notMnt, err := b.mounter.IsLikelyNotMountPoint(targetHostPath)
				if err == nil && notMnt {
					if mountErr := b.mounter.Mount(dir, targetHostPath, "", []string{"bind"}); mountErr != nil {
						klog.Errorf("Failed to bind-mount dynamic volume %s from %s to %s: %v", b.volName, dir, targetHostPath, mountErr)
						continue
					}
					klog.Infof("Dynamically hot-plugged volume %s from %s into %s (container path %s)", b.volName, dir, targetHostPath, targetMount.MountPath)
				}
				trackedPaths = append(trackedPaths, targetHostPath)
			}
		}
	}

	if len(trackedPaths) > 0 {
		trackingFile := dynamicMountsTrackingFile(dir, b.volName)
		_ = os.WriteFile(trackingFile, []byte(strings.Join(trackedPaths, "\n")+"\n"), 0600)
	}
}

func findParentHostVolumeDir(volumesRootDir, parentVolName string) string {
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

// MakePayload function is exported so that it can be called from the projection volume driver
func MakePayload(mappings []v1.KeyToPath, configMap *v1.ConfigMap, defaultMode *int32, defaultUser *int64, optional bool) (map[string]volumeutil.FileProjection, error) {
	if defaultMode == nil {
		return nil, fmt.Errorf("no defaultMode used, not even the default value for it")
	}

	payload := make(map[string]volumeutil.FileProjection, (len(configMap.Data) + len(configMap.BinaryData)))
	var fileProjection volumeutil.FileProjection

	if len(mappings) == 0 {
		for name, data := range configMap.Data {
			fileProjection.Data = []byte(data)
			fileProjection.Mode = *defaultMode
			if utilfeature.DefaultFeatureGate.Enabled(features.AtomicWriteVolumeUserFields) {
				fileProjection.FsUser = defaultUser
			}
			payload[name] = fileProjection
		}
		for name, data := range configMap.BinaryData {
			fileProjection.Data = data
			fileProjection.Mode = *defaultMode
			if utilfeature.DefaultFeatureGate.Enabled(features.AtomicWriteVolumeUserFields) {
				fileProjection.FsUser = defaultUser
			}
			payload[name] = fileProjection
		}
	} else {
		for _, ktp := range mappings {
			if stringData, ok := configMap.Data[ktp.Key]; ok {
				fileProjection.Data = []byte(stringData)
			} else if binaryData, ok := configMap.BinaryData[ktp.Key]; ok {
				fileProjection.Data = binaryData
			} else {
				if optional {
					continue
				}
				return nil, fmt.Errorf("configmap references non-existent config key: %s", ktp.Key)
			}

			fileProjection.FsUser = volumeutil.ResolvesFsUser(defaultUser, ktp.User)
			if ktp.Mode != nil {
				fileProjection.Mode = *ktp.Mode
			} else {
				fileProjection.Mode = *defaultMode
			}

			payload[ktp.Path] = fileProjection
		}
	}

	return payload, nil
}

func totalBytes(configMap *v1.ConfigMap) int {
	totalSize := 0
	for _, value := range configMap.Data {
		totalSize += len(value)
	}
	for _, value := range configMap.BinaryData {
		totalSize += len(value)
	}

	return totalSize
}

// configMapVolumeUnmounter handles cleaning up configMap volumes.
type configMapVolumeUnmounter struct {
	*configMapVolume
}

var _ volume.Unmounter = &configMapVolumeUnmounter{}

func (c *configMapVolumeUnmounter) TearDown() error {
	return c.TearDownAt(c.GetPath())
}

func (c *configMapVolumeUnmounter) TearDownAt(dir string) error {
	c.cleanupDynamicPropagationMounts(dir)
	return volumeutil.UnmountViaEmptyDir(dir, c.plugin.host, c.volName, wrappedVolumeSpec(), c.podUID)
}

func (c *configMapVolumeUnmounter) cleanupDynamicPropagationMounts(dir string) {
	if c.mounter == nil {
		return
	}
	trackingFile := dynamicMountsTrackingFile(dir, c.volName)
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
		if err := mount.CleanupMountPoint(targetHostPath, c.mounter, true); err != nil {
			klog.Warningf("Failed to cleanup dynamic propagation mount %s for volume %s: %v", targetHostPath, c.volName, err)
		} else {
			klog.Infof("Dynamically hot-unplugged propagated mount %s for volume %s", targetHostPath, c.volName)
		}
	}
	_ = os.Remove(trackingFile)
}

func getVolumeSource(spec *volume.Spec) (*v1.ConfigMapVolumeSource, bool) {
	var readOnly bool
	var volumeSource *v1.ConfigMapVolumeSource

	if spec.Volume != nil && spec.Volume.ConfigMap != nil {
		volumeSource = spec.Volume.ConfigMap
		readOnly = spec.ReadOnly
	}

	return volumeSource, readOnly
}
