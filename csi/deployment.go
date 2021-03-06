package csi

import (
	"sync"

	"github.com/Sirupsen/logrus"
	appsv1beta1 "k8s.io/api/apps/v1beta1"
	appsv1beta2 "k8s.io/api/apps/v1beta2"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"

	"github.com/rancher/longhorn-manager/util"
)

const (
	DefaultCSIAttacherImage        = "quay.io/k8scsi/csi-attacher:v0.4.0"
	DefaultCSIProvisionerImage     = "quay.io/k8scsi/csi-provisioner:v0.3.1"
	DefaultCSIDriverRegistrarImage = "quay.io/k8scsi/driver-registrar:v0.4.1"
	DefaultCSIProvisionerName      = "rancher.io/longhorn"
)

var (
	HostPathDirectory             = v1.HostPathDirectory
	HostPathDirectoryOrCreate     = v1.HostPathDirectoryOrCreate
	MountPropagationBidirectional = v1.MountPropagationBidirectional
)

type AttacherDeployment struct {
	service     *v1.Service
	statefulSet *appsv1beta1.StatefulSet
}

func NewAttacherDeployment(namespace, serviceAccount, attacherImage string) *AttacherDeployment {
	service := getCommonService("csi-attacher", namespace)

	statefulSet := getCommondStatefulSet(
		"csi-attacher",
		namespace,
		serviceAccount,
		attacherImage,
		[]string{
			"--v=5",
			"--csi-address=$(ADDRESS)",
		},
	)

	return &AttacherDeployment{
		service:     service,
		statefulSet: statefulSet,
	}
}

func (a *AttacherDeployment) Deploy(kubeClient *clientset.Clientset) error {
	if err := deployService(kubeClient, a.service); err != nil {
		return err
	}

	return deployStatefulSet(kubeClient, a.statefulSet)
}

func (a *AttacherDeployment) Cleanup(kubeClient *clientset.Clientset) {
	var wg sync.WaitGroup
	defer wg.Wait()

	util.RunAsync(&wg, func() {
		if err := cleanupService(kubeClient, a.service); err != nil {
			logrus.Warnf("Failed to cleanup Service in attacher deployment: %v", err)
		}
	})
	util.RunAsync(&wg, func() {
		if err := cleanupStatefulSet(kubeClient, a.statefulSet); err != nil {
			logrus.Warnf("Failed to cleanup StatefulSet in attacher deployment: %v", err)
		}
	})
}

type ProvisionerDeployment struct {
	service     *v1.Service
	statefulSet *appsv1beta1.StatefulSet
}

func NewProvisionerDeployment(namespace, serviceAccount, provisionerImage, provisionerName string) *ProvisionerDeployment {
	service := getCommonService("csi-provisioner", namespace)

	statefulSet := getCommondStatefulSet(
		"csi-provisioner",
		namespace,
		serviceAccount,
		provisionerImage,
		[]string{
			"--provisioner=" + provisionerName,
			"--csi-address=$(ADDRESS)",
			"--v=5",
		},
	)

	return &ProvisionerDeployment{
		service:     service,
		statefulSet: statefulSet,
	}
}

func (p *ProvisionerDeployment) Deploy(kubeClient *clientset.Clientset) error {
	if err := deployService(kubeClient, p.service); err != nil {
		return err
	}

	return deployStatefulSet(kubeClient, p.statefulSet)
}

func (p *ProvisionerDeployment) Cleanup(kubeClient *clientset.Clientset) {
	var wg sync.WaitGroup
	defer wg.Wait()

	util.RunAsync(&wg, func() {
		if err := cleanupService(kubeClient, p.service); err != nil {
			logrus.Warnf("Failed to cleanup Service in provisioner deployment: %v", err)
		}
	})
	util.RunAsync(&wg, func() {
		if err := cleanupStatefulSet(kubeClient, p.statefulSet); err != nil {
			logrus.Warnf("Failed to cleanup StatefulSet in provisioner deployment: %v", err)
		}
	})
}

type PluginDeployment struct {
	daemonSet *appsv1beta2.DaemonSet
}

func NewPluginDeployment(namespace, serviceAccount, driverRegistrarImage, managerImage, managerURL string, kubeletPluginWatcherEnabled bool) *PluginDeployment {
	args := []string{
		"--v=5",
		"--csi-address=$(ADDRESS)",
	}
	volumeMounts := []v1.VolumeMount{
		{
			Name:      "socket-dir",
			MountPath: "/var/lib/kubelet/plugins/io.rancher.longhorn",
		},
	}
	volumes := []v1.Volume{
		{
			Name: "plugin-dir",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/plugins/io.rancher.longhorn",
					Type: &HostPathDirectoryOrCreate,
				},
			},
		},
		{
			Name: "pods-mount-dir",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/pods",
					Type: &HostPathDirectoryOrCreate,
				},
			},
		},
		{
			Name: "socket-dir",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/plugins/io.rancher.longhorn",
					Type: &HostPathDirectoryOrCreate,
				},
			},
		},
		{
			Name: "host-dev",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/dev",
				},
			},
		},
		{
			Name: "host-sys",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/sys",
				},
			},
		},
		{
			Name: "lib-modules",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/lib-modules",
				},
			},
		},
	}

	// for Kubernetes v1.12+
	if kubeletPluginWatcherEnabled {
		args = append(args, "--kubelet-registration-path=/var/lib/kubelet/plugins/io.rancher.longhorn/csi.sock")
		volumeMounts = append(volumeMounts, v1.VolumeMount{
			Name:      "registration-dir",
			MountPath: "/registration",
		})
		volumes = append(volumes, v1.Volume{
			Name: "registration-dir",
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: "/var/lib/kubelet/plugins/",
					Type: &HostPathDirectory,
				},
			},
		})
	}

	daemonSet := &appsv1beta2.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "longhorn-csi-plugin",
			Namespace: namespace,
		},

		Spec: appsv1beta2.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "longhorn-csi-plugin",
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "longhorn-csi-plugin",
					},
				},
				Spec: v1.PodSpec{
					ServiceAccountName: serviceAccount,
					Containers: []v1.Container{
						{
							Name:  "driver-registrar",
							Image: driverRegistrarImage,
							Args:  args,
							Env: []v1.EnvVar{
								{
									Name:  "ADDRESS",
									Value: "/var/lib/kubelet/plugins/io.rancher.longhorn/csi.sock",
								},
								{
									Name: "KUBE_NODE_NAME",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							//ImagePullPolicy: v1.PullAlways,
							VolumeMounts: volumeMounts,
						},
						{
							Name: "longhorn-csi-plugin",
							SecurityContext: &v1.SecurityContext{
								Privileged: pointer.BoolPtr(true),
								Capabilities: &v1.Capabilities{
									Add: []v1.Capability{
										"SYS_ADMIN",
									},
								},
								AllowPrivilegeEscalation: pointer.BoolPtr(true),
							},
							Image: managerImage,
							Args: []string{
								"longhorn-manager",
								"-d",
								"csi",
								"--nodeid=$(NODE_ID)",
								"--endpoint=$(CSI_ENDPOINT)",
								"--drivername=io.rancher.longhorn",
								"--manager-url=" + managerURL,
							},
							Env: []v1.EnvVar{
								{
									Name: "NODE_ID",
									ValueFrom: &v1.EnvVarSource{
										FieldRef: &v1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
								{
									Name:  "CSI_ENDPOINT",
									Value: "unix://var/lib/kubelet/plugins/io.rancher.longhorn/csi.sock",
								},
							},
							VolumeMounts: []v1.VolumeMount{
								{
									Name:      "plugin-dir",
									MountPath: "/var/lib/kubelet/plugins/io.rancher.longhorn",
								},
								{
									Name:             "pods-mount-dir",
									MountPath:        "/var/lib/kubelet/pods",
									MountPropagation: &MountPropagationBidirectional,
								},
								{
									Name:      "host-dev",
									MountPath: "/dev",
								},
								{
									Name:      "host-sys",
									MountPath: "/sys",
								},
								{
									Name:      "lib-modules",
									MountPath: "/lib/modules",
									ReadOnly:  true,
								},
							},
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	return &PluginDeployment{
		daemonSet: daemonSet,
	}
}

func (p *PluginDeployment) Deploy(kubeClient *clientset.Clientset) error {
	return deployDaemonSet(kubeClient, p.daemonSet)
}

func (p *PluginDeployment) Cleanup(kubeClient *clientset.Clientset) {
	if err := cleanupDaemonSet(kubeClient, p.daemonSet); err != nil {
		logrus.Warnf("Failed to cleanup DaemonSet in plugin deployment: %v", err)
	}
}
