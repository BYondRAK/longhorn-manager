package controller

import (
	"fmt"

	"github.com/Sirupsen/logrus"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/rancher/longhorn-manager/types"
)

const (
	CrashLogsTaillines = 100
)

// InstanceHandler can handle the state transition of correlated instance and
// engine/replica object. It assumed the pod it's going to operate with is using
// the SAME NAME from the engine/replica object
type InstanceHandler struct {
	namespace     string
	kubeClient    clientset.Interface
	pLister       corelisters.PodLister
	podCreator    PodCreatorInterface
	eventRecorder record.EventRecorder
}

type PodCreatorInterface interface {
	CreatePodSpec(obj interface{}) (*v1.Pod, error)
}

func NewInstanceHandler(podInformer coreinformers.PodInformer, kubeClient clientset.Interface, namespace string, podCreator PodCreatorInterface, eventRecorder record.EventRecorder) *InstanceHandler {
	return &InstanceHandler{
		namespace:     namespace,
		kubeClient:    kubeClient,
		pLister:       podInformer.Lister(),
		podCreator:    podCreator,
		eventRecorder: eventRecorder,
	}
}

func (h *InstanceHandler) syncStatusWithPod(pod *v1.Pod, spec *types.InstanceSpec, status *types.InstanceStatus) {
	if pod == nil {
		if status.Started {
			status.CurrentState = types.InstanceStateError
			status.IP = ""
			status.CurrentImage = ""
		} else {
			status.CurrentState = types.InstanceStateStopped
			status.IP = ""
			status.CurrentImage = ""
		}
		return
	}

	if pod.DeletionTimestamp != nil {
		status.CurrentState = types.InstanceStateStopping
		status.IP = ""
		status.CurrentImage = ""
		return
	}

	switch pod.Status.Phase {
	case v1.PodPending:
		status.CurrentState = types.InstanceStateStarting
		status.IP = ""
		status.CurrentImage = ""
	case v1.PodRunning:
		for _, st := range pod.Status.ContainerStatuses {
			// wait until all containers passed readiness probe
			if !st.Ready {
				status.CurrentState = types.InstanceStateStarting
				status.IP = ""
				status.CurrentImage = ""
				return
			}
		}
		status.CurrentState = types.InstanceStateRunning
		if status.IP != pod.Status.PodIP {
			status.IP = pod.Status.PodIP
			logrus.Debugf("Instance %v starts running, IP %v", pod.Name, status.IP)
		}
		// only set CurrentImage when first started, since later we may specify
		// different spec.EngineImage for upgrade
		if status.CurrentImage == "" {
			status.CurrentImage = spec.EngineImage
		}
		nodeBootID, err := h.GetNodeBootIDForPod(pod)
		if err != nil {
			logrus.Warnf("cannot get node BootID for instance %v", pod.Name)
		} else {
			status.NodeBootID = nodeBootID
		}
	default:
		logrus.Warnf("instance %v state is failed/unknown, pod state %v",
			pod.Name, pod.Status.Phase)
		status.CurrentState = types.InstanceStateError
		status.IP = ""
		status.CurrentImage = ""
		// Don't reset status.NodeBootID, we need it to identify a node reboot
	}
}

// getNameFromObj will get the name from the object metadata, which will be used
// as podName later
func (h *InstanceHandler) getNameFromObj(obj runtime.Object) (string, error) {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return "", err
	}
	return metadata.GetName(), nil
}

func (h *InstanceHandler) ReconcileInstanceState(obj interface{}, spec *types.InstanceSpec, status *types.InstanceStatus) (err error) {
	runtimeObj, ok := obj.(runtime.Object)
	if !ok {
		return fmt.Errorf("obj is not a runtime.Object: %v", obj)
	}
	podName, err := h.getNameFromObj(runtimeObj)
	if err != nil {
		return err
	}

	pod, err := h.getPod(podName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		pod = nil
	}

	switch spec.DesireState {
	case types.InstanceStateRunning:
		if pod != nil && pod.DeletionTimestamp == nil {
			status.Started = true
			break
		}
		if status.CurrentState != types.InstanceStateStopped {
			break
		}
		podSpec, err := h.podCreator.CreatePodSpec(obj)
		if err != nil {
			return err
		}
		pod, err = h.createPodForObject(runtimeObj, podSpec)
		if err != nil {
			return err
		}
	case types.InstanceStateStopped:
		if pod != nil && pod.DeletionTimestamp == nil {
			if err := h.deletePodForObject(runtimeObj); err != nil {
				return err
			}
		}
		status.Started = false
		status.NodeBootID = ""
	default:
		return fmt.Errorf("BUG: unknown instance desire state: desire %v", spec.DesireState)
	}

	h.syncStatusWithPod(pod, spec, status)

	if status.CurrentState == types.InstanceStateRunning {
		// pin down to this node ID. it's needed for a replica and
		// engine should specify nodeName as well
		if spec.NodeID == "" {
			spec.NodeID = pod.Spec.NodeName
		} else if spec.NodeID != pod.Spec.NodeName {
			status.CurrentState = types.InstanceStateError
			status.IP = ""
			status.NodeBootID = ""
			err := fmt.Errorf("BUG: instance %v wasn't pin down to the host %v", pod.Name, spec.NodeID)
			logrus.Errorf("%v", err)
			return err
		}
	} else if status.CurrentState == types.InstanceStateError && pod != nil {
		logs, err := h.getPodLogs(pod.Name, CrashLogsTaillines)
		if err == nil {
			logrus.Warnf("instance %v crashed, log: \n%v", pod.Name, logs)
		} else {
			logrus.Warnf("instance %v crashed, but cannot get log, error %v", pod.Name, err)
		}
	}
	return nil
}

func (h *InstanceHandler) getPod(podName string) (*v1.Pod, error) {
	return h.pLister.Pods(h.namespace).Get(podName)
}

func (h *InstanceHandler) getPodLogs(podName string, taillines int) (string, error) {
	tails := int64(taillines)
	req := h.kubeClient.CoreV1().Pods(h.namespace).GetLogs(podName, &v1.PodLogOptions{
		Timestamps: true,
		TailLines:  &tails,
	})
	if req.URL().Path == "" {
		return "", fmt.Errorf("GetLogs for %v/%v returns empty request path, may due to unit test run: %+v", h.namespace, podName, req)
	}

	logs, err := req.DoRaw()
	if err != nil {
		return "", err
	}
	return string(logs), nil
}

func (h *InstanceHandler) createPodForObject(obj runtime.Object, pod *v1.Pod) (*v1.Pod, error) {
	p, err := h.kubeClient.CoreV1().Pods(h.namespace).Create(pod)
	if err != nil {
		h.eventRecorder.Eventf(obj, v1.EventTypeWarning, EventReasonFailedStarting, "Error starting %v: %v", pod.Name, err)
		return nil, err
	}
	h.eventRecorder.Eventf(obj, v1.EventTypeNormal, EventReasonStart, "Starts %v", pod.Name)
	return p, nil
}

func (h *InstanceHandler) deletePodForObject(obj runtime.Object) error {
	podName, err := h.getNameFromObj(obj)
	if err != nil {
		return err
	}

	if err := h.kubeClient.CoreV1().Pods(h.namespace).Delete(podName, nil); err != nil {
		h.eventRecorder.Eventf(obj, v1.EventTypeWarning, EventReasonFailedStopping, "Error stopping %v: %v", podName, err)
		return nil
	}
	h.eventRecorder.Eventf(obj, v1.EventTypeNormal, EventReasonStop, "Stops %v", podName)
	return nil
}

func (h *InstanceHandler) DeleteInstanceForObject(obj runtime.Object) (err error) {
	podName, err := h.getNameFromObj(obj)
	if err != nil {
		return err
	}

	pod, err := h.getPod(podName)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// pod already stopped
	if apierrors.IsNotFound(err) {
		return nil
	}
	// pod has been already asked to stop
	if pod.DeletionTimestamp != nil {
		return nil
	}
	return h.deletePodForObject(obj)
}

func (h *InstanceHandler) GetNodeBootIDForPod(pod *v1.Pod) (string, error) {
	nodeName := pod.Spec.NodeName
	node, err := h.kubeClient.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return node.Status.NodeInfo.BootID, nil
}
