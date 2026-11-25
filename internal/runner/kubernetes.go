package runner

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Kubernetes runs backends as Pods on molt's own node, each with its
// generation's directory from the node's disk (a hostPath: molt mounts the
// same path, so the directory has one name everywhere).
type Kubernetes struct {
	Client    kubernetes.Interface
	Namespace string
	Prefix    string
	// Node is where backends run: molt's node, whose disk holds the worlds.
	Node string
	// Memory is each backend's memory, requested and limited alike.
	Memory resource.Quantity
	// UID runs the server; the world's files belong to it.
	UID int64
}

const genLabel = "molt.dev/gen"

func (k *Kubernetes) Start(ctx context.Context, gen int, dir string, spec Spec) (Backend, error) {
	hostPath := corev1.HostPathDirectory
	no := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   Name(k.Prefix, gen),
			Labels: map[string]string{"app.kubernetes.io/name": "molt-backend", genLabel: strconv.Itoa(gen)},
		},
		Spec: corev1.PodSpec{
			NodeSelector:                  map[string]string{"kubernetes.io/hostname": k.Node},
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: new(int64(90)),
			AutomountServiceAccountToken:  &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsUser: &k.UID, RunAsGroup: &k.UID, RunAsNonRoot: new(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:  "server",
				Image: spec.Image,
				Env:   podEnv(spec),
				Ports: []corev1.ContainerPort{
					{Name: "game", ContainerPort: GamePort},
					{Name: "agent", ContainerPort: AgentPort},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: k.Memory, corev1.ResourceCPU: resource.MustParse("500m")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: k.Memory},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/data"}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: dir, Type: &hostPath}},
			}},
		},
	}
	if _, err := k.Client.CoreV1().Pods(k.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return Backend{}, err
	}
	for {
		b, ok, err := k.Find(ctx, gen)
		if ok || err != nil {
			return b, err
		}
		select {
		case <-ctx.Done():
			return Backend{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func podEnv(spec Spec) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, kv := range env(spec) {
		k, v, _ := strings.Cut(kv, "=")
		out = append(out, corev1.EnvVar{Name: k, Value: v})
	}
	return out
}

func (k *Kubernetes) Stop(ctx context.Context, b Backend) error {
	pods := k.Client.CoreV1().Pods(k.Namespace)
	if err := pods.Delete(ctx, b.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Wait, so the server has saved and let go of its world.
	for {
		if _, err := pods.Get(ctx, b.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (k *Kubernetes) Find(ctx context.Context, gen int) (Backend, bool, error) {
	name := Name(k.Prefix, gen)
	pod, err := k.Client.CoreV1().Pods(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Backend{}, false, nil
	}
	if err != nil {
		return Backend{}, false, err
	}
	switch pod.Status.Phase {
	case corev1.PodFailed, corev1.PodSucceeded:
		return Backend{}, false, fmt.Errorf("%s has stopped (%s)", name, pod.Status.Phase)
	}
	if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" {
		return Backend{}, false, nil
	}
	ip := pod.Status.PodIP
	return Backend{
		Gen: gen, Name: name,
		Game:  net.JoinHostPort(ip, strconv.Itoa(GamePort)),
		Agent: "http://" + net.JoinHostPort(ip, strconv.Itoa(AgentPort)),
	}, true, nil
}
