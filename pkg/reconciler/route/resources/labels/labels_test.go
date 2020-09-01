/*
Copyright 2019 The Knative Authors

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

package labels

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"knative.dev/serving/pkg/apis/serving"
)

func TestIsObjectLocalVisibility(t *testing.T) {
	tests := []struct {
		name     string
		meta     *v1.ObjectMeta
		expected bool
	}{{
		name: "nil",
		meta: &v1.ObjectMeta{},
	}, {
		name: "empty labels",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{},
		},
	}, {
		name: "no matching labels",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{"frankie-goes": "to-hollywood"},
		},
	}, {
		name: "false",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{serving.VisibilityLabelKey: ""},
		},
	}, {
		name: "true",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{serving.VisibilityLabelKey: "set"},
		},
		expected: true,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, want := IsObjectLocalVisibility(test.meta), test.expected; got != want {
				t.Errorf("IsObjectLocalVisibility = %v, want: %v", got, want)
			}
		})
	}
}

func TestDeleteLabel(t *testing.T) {
	tests := []struct {
		name     string
		meta     *v1.ObjectMeta
		key      string
		expected v1.ObjectMeta
	}{{
		name:     "No labels in object meta",
		meta:     &v1.ObjectMeta{},
		key:      "key",
		expected: v1.ObjectMeta{},
	}, {
		name: "No matching key",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{"some label": "some value"},
		},
		key: "unknown",
		expected: v1.ObjectMeta{
			Labels: map[string]string{"some label": "some value"},
		},
	}, {
		name: "Has matching key",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{"some label": "some value"},
		},
		key: "some label",
		expected: v1.ObjectMeta{
			Labels: map[string]string{},
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			DeleteLabel(tt.meta, tt.key)

			if !cmp.Equal(tt.expected, *tt.meta) {
				t.Errorf("DeleteLabel (-want, +got) = %v",
					cmp.Diff(tt.expected, *tt.meta))
			}
		})
	}
}

func TestSetLabel(t *testing.T) {
	tests := []struct {
		name  string
		meta  *v1.ObjectMeta
		key   string
		value string

		expected v1.ObjectMeta
	}{{
		name:  "No labels in object meta",
		meta:  &v1.ObjectMeta{},
		key:   "key",
		value: "value",
		expected: v1.ObjectMeta{
			Labels: map[string]string{"key": "value"},
		},
	}, {
		name: "Empty labels",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{},
		},
		key:   "key",
		value: "value",
		expected: v1.ObjectMeta{
			Labels: map[string]string{"key": "value"},
		},
	}, {
		name: "Conflicting labels",
		meta: &v1.ObjectMeta{
			Labels: map[string]string{"key": "old value"},
		},
		key:   "key",
		value: "new value",
		expected: v1.ObjectMeta{
			Labels: map[string]string{"key": "new value"},
		},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetLabel(tt.meta, tt.key, tt.value)

			if !cmp.Equal(tt.expected, *tt.meta) {
				t.Errorf("DeleteLabel (-want, +got) = %v",
					cmp.Diff(tt.expected, *tt.meta))
			}
		})
	}
}

func TestSetVisibility(t *testing.T) {
	tests := []struct {
		name           string
		meta           *v1.ObjectMeta
		isClusterLocal bool
		expected       v1.ObjectMeta
	}{{
		name:           "Set cluster local true",
		meta:           &v1.ObjectMeta{},
		isClusterLocal: true,
		expected:       v1.ObjectMeta{Labels: map[string]string{serving.VisibilityLabelKey: serving.VisibilityClusterLocal}},
	}, {
		name:           "Set cluster local false",
		meta:           &v1.ObjectMeta{Labels: map[string]string{serving.VisibilityLabelKey: serving.VisibilityClusterLocal}},
		isClusterLocal: false,
		expected:       v1.ObjectMeta{Labels: map[string]string{}},
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetVisibility(tt.meta, tt.isClusterLocal)
		})
	}
}
