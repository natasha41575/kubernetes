/*
Copyright 2021 The Kubernetes Authors.

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
	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
)

var _ checkpointmanager.Checkpoint = &Checkpoint{}

// Checkpoint represents a structure to store pod resource allocation checkpoint data as a Protobuf PodList.
type Checkpoint struct {
	// PodList is a serialized list of pods on the node.
	PodList *v1.PodList
}

// NewCheckpoint creates a new checkpoint from a PodList
func NewCheckpoint(podList *v1.PodList) *Checkpoint {
	return &Checkpoint{
		PodList: podList,
	}
}

func (cp *Checkpoint) MarshalCheckpoint() ([]byte, error) {
	if cp.PodList == nil {
		cp.PodList = &v1.PodList{}
	}
	return cp.PodList.Marshal()
}

// UnmarshalCheckpoint unmarshals checkpoint from Protobuf.
func (cp *Checkpoint) UnmarshalCheckpoint(blob []byte) error {
	var podList v1.PodList
	if err := podList.Unmarshal(blob); err != nil {
		return err
	}
	cp.PodList = &podList
	return nil
}

// VerifyChecksum is a no-op for protobuf checkpoints.
func (cp *Checkpoint) VerifyChecksum() error {
	return nil
}
