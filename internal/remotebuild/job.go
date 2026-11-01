package remotebuild

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const appLabel = "beekeeper-builder"

// Job gives each worktree/toolchain a warm workspace without cluster credentials.
// Exiting the watchdog lets the TTL controller delete both Job and pod.
func Job(c Config, name, image string) *batchv1.Job {
	c = c.Defaults()
	cpu := resource.MustParse(c.CPU)
	cacheSpec := ""
	if c.Cache.Enabled {
		raw, _ := json.Marshal(struct {
			Config Cache
			Env    []corev1.EnvVar
		}{c.Cache, c.Cache.env()})
		cacheSpec = "\x00" + string(raw)
	}
	labels := map[string]string{"app.kubernetes.io/name": appLabel}
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace, Labels: labels, Annotations: map[string]string{"beekeeper.dev/builder-spec": fmt.Sprintf("%x", sha256.Sum256([]byte(image+"\x00"+c.CPU+"\x00"+c.Memory+"\x00"+c.IdleTimeout.String()+"\x00"+watchdog+cacheSpec)))}},
		Spec: batchv1.JobSpec{BackoffLimit: ptr.To(int32(0)), TTLSecondsAfterFinished: ptr.To(int32(60)), Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, AutomountServiceAccountToken: ptr.To(false), TerminationGracePeriodSeconds: ptr.To(int64(10)),
				NodeSelector:    map[string]string{"kubernetes.io/arch": "amd64"},
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)), RunAsGroup: ptr.To(int64(1000)), FSGroup: ptr.To(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Containers: []corev1.Container{{Name: "builder", Image: image, Command: []string{shellBash, "-c", watchdog}, Env: append([]corev1.EnvVar{
					{Name: "IDLE", Value: strconv.FormatInt(int64(c.IdleTimeout.Seconds()), 10)},
					{Name: "GOCACHE", Value: "/work/go-cache"}, {Name: "GOMODCACHE", Value: "/work/go-mod"},
					{Name: "CARGO_TARGET_DIR", Value: "/work/target"}, {Name: "CARGO_HOME", Value: "/work/cargo"},
					{Name: "GOMAXPROCS", Value: strconv.FormatInt(max(1, cpu.MilliValue()/1000), 10)},
					{Name: "CARGO_BUILD_JOBS", Value: strconv.FormatInt(max(1, cpu.MilliValue()/1000), 10)},
				}, c.Cache.env()...), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(c.CPU), corev1.ResourceMemory: resource.MustParse(c.Memory)}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(c.CPU), corev1.ResourceMemory: resource.MustParse(c.Memory)}},
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
					VolumeMounts:    []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
				}}, Volumes: []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("30Gi"))}}}},
			},
		}},
	}
}

const watchdog = `set -eu
mkdir -p /work/runs /work/src; touch /work/active
while :; do
 # Recover a client that vanished during upload, before launching a wrapper.
 if [ -d /work/busy ] && [ $(( $(date +%s) - $(stat -c %Y /work/active) )) -ge 60 ]; then
  id=$(cat /work/busy/id 2>/dev/null || true)
  if [ -n "$id" ] && ! kill -0 "$(cat /work/busy/owner 2>/dev/null)" 2>/dev/null; then
   # An orphaned command must stop before another run can replace its source.
   p=$(cat "/work/runs/$id/child" 2>/dev/null || true)
   case "$p" in ''|*[!0-9]*) ;; *) kill -KILL -- "-$p" 2>/dev/null || true;; esac
   printf '137\n' > "/work/runs/$id/rc.tmp"
   mv "/work/runs/$id/rc.tmp" "/work/runs/$id/rc"
   rm -rf /work/busy /work/incoming
  fi
 fi
 if [ ! -d /work/busy ] && [ $(( $(date +%s) - $(stat -c %Y /work/active) )) -ge "$IDLE" ]; then exit 0; fi
 sleep 10
done`
