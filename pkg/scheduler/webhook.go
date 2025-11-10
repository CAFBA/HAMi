/*
Copyright 2024 The HAMi Authors.

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

package scheduler

import (
	"context"
	"encoding/json"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/scheduler/config"
)

const template = "Processing admission hook for pod %v/%v, UID: %v"

type webhook struct {
	decoder admission.Decoder
}

func NewWebHook() (*admission.Webhook, error) {
	logf.SetLogger(klog.NewKlogr())
	schema := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(schema); err != nil {
		return nil, err
	}
	decoder := admission.NewDecoder(schema)
	wh := &admission.Webhook{Handler: &webhook{decoder: decoder}}
	return wh, nil
}
/**
 * * my
 * 判断 Pod 是否需要使用 HAMi-Scheduler 进行调度
 * 需要的话就修改 Pod 的 SchedulerName 字段为 hami-scheduler(名字可配置)
 */
func (h *webhook) Handle(_ context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	err := h.decoder.Decode(req, pod)
	if err != nil {
		klog.Errorf("Failed to decode request: %v", err)
		return admission.Errored(http.StatusBadRequest, err)
	}
	if len(pod.Spec.Containers) == 0 {
		klog.Warningf(template+" - Denying admission as pod has no containers", pod.Namespace, pod.Name, pod.UID)
		return admission.Denied("pod has no containers")
	}
	if pod.Spec.SchedulerName != "" &&
		pod.Spec.SchedulerName != corev1.DefaultSchedulerName || !config.ForceOverwriteDefaultScheduler &&
		(len(config.SchedulerName) == 0 || pod.Spec.SchedulerName != config.SchedulerName) {
		klog.Infof(template+" - Pod already has different scheduler assigned", req.Namespace, req.Name, req.UID)
		return admission.Allowed("pod already has different scheduler assigned")
	}
	klog.Infof(template, pod.Namespace, pod.Name, pod.UID)
	hasResource := false
	for idx, ctr := range pod.Spec.Containers {
		c := &pod.Spec.Containers[idx]
		// 对于特权模式的 Pod，HAMi 直接忽略，因为开启特权模式之后，Pod 可以访问宿主机上的所有设备，再做限制也没意义
		if ctr.SecurityContext != nil {
			if ctr.SecurityContext.Privileged != nil && *ctr.SecurityContext.Privileged {
				klog.Warningf(template+" - Denying admission as container %s is privileged", pod.Namespace, pod.Name, pod.UID, c.Name)
				continue
			}
		}
		// 如果 Pod Resource 中有申请 HAMi 这边支持的 vGPU 资源，则需要使用 HAMi-Scheduler 进行调度
		// devices 是一个全局变量， 在 cmd/scheduler/main.go 中通过 InitDevices 初始化
		for _, val := range device.GetDevices() {
			// 具体的判断逻辑取决于每个硬件厂商自己的 MutateAdmission 实现
			found, err := val.MutateAdmission(c, pod)
			if err != nil {
				klog.Errorf("validating pod failed:%s", err.Error())
				return admission.Errored(http.StatusInternalServerError, err)
			}
			hasResource = hasResource || found
		}
	}
	// 对于上述满足条件的 Pod，需要由 HAMi-Scheduler 进行调度，Webhook 中会将 Pod 的 spec.schedulerName 改成 hami-scheduler
	if !hasResource {
		klog.Infof(template+" - Allowing admission for pod: no resource found", pod.Namespace, pod.Name, pod.UID)
		//return admission.Allowed("no resource found")
	} else if len(config.SchedulerName) > 0 {
		pod.Spec.SchedulerName = config.SchedulerName
		// 对于使用 vGPU 资源但直接指定 nodeName 的 Pod，Webhook 会直接拒绝，拦截掉 Pod 的创建
		// 因为指定 nodeName 说明 Pod 不需要调度，会直接到指定节点启动，但是没经过调度，可能该节点并没有足够的资源
		if pod.Spec.NodeName != "" {
			klog.Infof(template+" - Pod already has node assigned", pod.Namespace, pod.Name, pod.UID)
			return admission.Denied("pod has node assigned")
		}
	}
	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		klog.Errorf(template+" - Failed to marshal pod, error: %v", pod.Namespace, pod.Name, pod.UID, err)
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}
