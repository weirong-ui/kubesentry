/*
Copyright 2026 weirong-ui

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

// 本文件只放 GVK / Scheme 注册，包级文档注释与 kubebuilder 标记在 app_types.go。
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion 是本 API 组的唯一版本入口，CRD / RBAC 生成都依赖它。
	GroupVersion = schema.GroupVersion{Group: "apps.sentry.io", Version: "v1alpha1"}

	// SchemeBuilder 收集需要在 Scheme 中注册的类型。
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme 把本组类型注册进给定的 Scheme。
	AddToScheme = SchemeBuilder.AddToScheme
)
