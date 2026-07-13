// Package up renders luncur's own self-deploy manifests: the ServiceAccount,
// ClusterRoleBinding, Deployment, Service, and Ingress that run luncur itself
// on the K3s cluster it manages.
package up

import (
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/sutantodadang/luncur/internal/render"
)

const systemNamespace = "luncur-system"

// BootstrapSecretName is the Secret luncur's Deployment reads the initial
// admin bootstrap token from (key "admin").
const BootstrapSecretName = "luncur-bootstrap"

// LitestreamSecretName is the Secret the litestream sidecar reads its S3
// credentials from (keys "access-key" and "secret-key").
const LitestreamSecretName = "luncur-litestream"

// litestreamImage is pinned: the sidecar shares the data volume with the
// server, so bump deliberately, not via :latest.
const litestreamImage = "litestream/litestream:0.3"

// SSHNodePort is where git push reaches the in-cluster SSH receiver:
// ssh://git@<ip>:30022/<project>/<app>.git
const SSHNodePort = 30022

// Params configures luncur's own self-deploy manifests.
type Params struct {
	Image        string // luncur server image
	ExternalIP   string
	BuilderImage string
	CertProvider string // builtin|traefik|cert-manager
	ACMEEmail    string

	// ReplicaURL enables the Litestream sidecar when non-empty: the
	// replica destination, e.g. "s3://bucket/luncur". ReplicaEndpoint
	// optionally targets a non-AWS S3 endpoint.
	ReplicaURL      string
	ReplicaEndpoint string
}

func ptr[T any](v T) *T { return &v }

// PanelHost returns the sslip.io host luncur's own panel Ingress is served
// on, derived from the cluster's external IP.
func PanelHost(ip string) string {
	return "panel." + ip + ".sslip.io"
}

// luncurLabels are the standard labels stamped on every object LuncurObjects
// and LuncurClusterRole render.
func luncurLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "luncur",
		"app.kubernetes.io/managed-by": "luncur",
	}
}

func rule(groups []string, resources []string, verbs ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: groups, Resources: resources, Verbs: verbs}
}

// LuncurClusterRole returns the "luncur" ClusterRole — the single source of
// truth for LuncurObjects (applied by `luncur up`) and the server's own
// startup self-heal (internal/kube.EnsureClusterRole, wired in
// internal/cli/serve.go): every release that adds a permission the server
// needs updates this one function, and both paths pick it up.
//
// The last rule grants a narrow, scoped "escalate" on this ClusterRole
// itself (via ResourceNames), so the server's own ServiceAccount can extend
// its role's rules without cluster-admin: Kubernetes' RBAC escalation
// prevention otherwise blocks a ServiceAccount from granting rules it
// doesn't already hold, which is exactly what self-heal needs to do when a
// new release adds a permission.
func LuncurClusterRole() *rbacv1.ClusterRole {
	labels := luncurLabels()
	full := []string{"get", "list", "watch", "create", "update", "patch", "delete"}
	read := []string{"get", "list", "watch"}
	manage := []string{"get", "list", "watch", "create", "update", "patch"}
	return &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur", Labels: labels},
		Rules: []rbacv1.PolicyRule{
			rule([]string{""}, []string{"namespaces", "services", "secrets", "configmaps", "serviceaccounts", "persistentvolumeclaims", "resourcequotas", "limitranges"}, full...),
			rule([]string{""}, []string{"pods", "pods/log", "events", "nodes"}, read...),
			rule([]string{""}, []string{"pods/exec"}, "create"),
			rule([]string{"apps"}, []string{"deployments", "statefulsets", "daemonsets"}, full...),
			rule([]string{"apps"}, []string{"replicasets"}, read...),
			// jobs needs deletecollection on top of full: DeleteAppObjects
			// removes an app's per-run Jobs by label via DeleteCollection.
			rule([]string{"batch"}, []string{"jobs", "cronjobs"}, append(full, "deletecollection")...),
			rule([]string{"networking.k8s.io"}, []string{"ingresses", "networkpolicies"}, full...),
			rule([]string{"helm.cattle.io"}, []string{"helmchartconfigs"}, manage...),
			rule([]string{"node.k8s.io"}, []string{"runtimeclasses"}, manage...),
			rule([]string{"cert-manager.io"}, []string{"clusterissuers"}, manage...),
			rule([]string{"metrics.k8s.io"}, []string{"pods", "nodes"}, read...),
			rule([]string{"autoscaling"}, []string{"horizontalpodautoscalers"}, full...),
			rule([]string{"policy"}, []string{"poddisruptionbudgets"}, full...),
			{
				APIGroups:     []string{"rbac.authorization.k8s.io"},
				Resources:     []string{"clusterroles"},
				ResourceNames: []string{"luncur"},
				Verbs:         []string{"get", "update", "patch", "escalate"},
			},
		},
	}
}

// LuncurObjects returns the manifests luncur applies to deploy itself:
// ServiceAccount, ClusterRoleBinding, Deployment, Service, and Ingress —
// all in the luncur-system namespace, labeled app.kubernetes.io/managed-by:
// luncur.
func LuncurObjects(p Params) ([]render.Object, error) {
	var objs []render.Object
	add := func(kind string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		objs = append(objs, render.Object{Kind: kind, JSON: b})
		return nil
	}

	labels := luncurLabels()

	sa := &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur", Namespace: systemNamespace, Labels: labels},
	}
	if err := add("ServiceAccount", sa); err != nil {
		return nil, err
	}

	if err := add("ClusterRole", LuncurClusterRole()); err != nil {
		return nil, err
	}

	// Scoped role: rules enumerate exactly what serve touches; extend here
	// when a new kind is applied.
	crb := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur-admin", Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "luncur"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "luncur", Namespace: systemNamespace}},
	}
	if err := add("ClusterRoleBinding", crb); err != nil {
		return nil, err
	}

	args := []string{
		"serve",
		"--listen", ":8080",
		"--db", "/var/lib/luncur/luncur.db",
		"--data-dir", "/var/lib/luncur/data",
		"--secret-key-file", "/var/lib/luncur/luncur.key",
		"--external-ip", p.ExternalIP,
		"--builder-image", p.BuilderImage,
		"--bootstrap-admin", "$(BOOTSTRAP_ADMIN)",
		"--ssh-listen", ":2222",
		"--cert-provider", p.CertProvider,
	}
	if p.ACMEEmail != "" {
		args = append(args, "--acme-email", p.ACMEEmail)
	}

	containers := []corev1.Container{{
		Name:  "luncur",
		Image: p.Image,
		Args:  args,
		Env: []corev1.EnvVar{{
			Name: "BOOTSTRAP_ADMIN",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: BootstrapSecretName},
				Key:                  "admin",
			}},
		}},
		Ports: []corev1.ContainerPort{{ContainerPort: 8080}, {ContainerPort: 2222}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "luncur-data", MountPath: "/var/lib/luncur"},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/v1/health",
					Port: intstr.FromInt32(8080),
				},
			},
		},
		// A hung server (deadlocked, out of file descriptors, ...) still
		// answers TCP but never HTTP — liveness restarts it so the kubelet
		// self-heals instead of leaving a dead panel up indefinitely.
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/v1/health",
					Port: intstr.FromInt32(8080),
				},
			},
			InitialDelaySeconds: 10,
			PeriodSeconds:       10,
			FailureThreshold:    3,
		},
	}}

	volumes := []corev1.Volume{{
		Name: "luncur-data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: "luncur-data",
			},
		},
	}}

	if p.ReplicaURL != "" {
		cfg := "dbs:\n  - path: /var/lib/luncur/luncur.db\n    replicas:\n      - url: " + p.ReplicaURL + "\n"
		if p.ReplicaEndpoint != "" {
			cfg += "        endpoint: " + p.ReplicaEndpoint + "\n"
		}
		cm := &corev1.ConfigMap{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "luncur-litestream",
				Namespace: systemNamespace,
				Labels:    labels,
			},
			Data: map[string]string{"litestream.yml": cfg},
		}
		if err := add("ConfigMap", cm); err != nil {
			return nil, err
		}

		containers = append(containers, corev1.Container{
			Name:  "litestream",
			Image: litestreamImage,
			Args:  []string{"replicate", "-config", "/etc/litestream/litestream.yml"},
			Env: []corev1.EnvVar{
				{Name: "LITESTREAM_ACCESS_KEY_ID", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: LitestreamSecretName}, Key: "access-key"}}},
				{Name: "LITESTREAM_SECRET_ACCESS_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: LitestreamSecretName}, Key: "secret-key"}}},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "luncur-data", MountPath: "/var/lib/luncur"},
				{Name: "litestream-config", MountPath: "/etc/litestream"},
			},
		})
		volumes = append(volumes, corev1.Volume{
			Name:         "litestream-config",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "luncur-litestream"}}},
		})
	}

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "luncur",
			Namespace: systemNamespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			// Recreate, not the default RollingUpdate: this is a
			// single-replica control plane backed by one RWO PVC holding a
			// SQLite file. RollingUpdate would briefly run old and new pods
			// together, both writing the same database file.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: "luncur",
					Containers:         containers,
					Volumes:            volumes,
				},
			},
		},
	}
	if err := add("Deployment", dep); err != nil {
		return nil, err
	}

	svc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "luncur",
			Namespace: systemNamespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/name": "luncur"},
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(8080),
			}},
		},
	}
	if err := add("Service", svc); err != nil {
		return nil, err
	}

	sshSvc := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "luncur-ssh",
			Namespace: systemNamespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: map[string]string{"app.kubernetes.io/name": "luncur"},
			Ports: []corev1.ServicePort{{
				Port:       2222,
				TargetPort: intstr.FromInt32(2222),
				NodePort:   SSHNodePort,
			}},
		},
	}
	if err := add("Service", sshSvc); err != nil {
		return nil, err
	}

	ingObj, err := PanelIngress(p.ExternalIP, "", "")
	if err != nil {
		return nil, err
	}
	objs = append(objs, ingObj)

	return objs, nil
}

// PanelIngress builds luncur's own panel Ingress: a base rule for the
// sslip.io host (PanelHost), plus — when customHost is set — a second rule
// for the custom domain sharing the same backend. tlsSecret (only meaningful
// alongside a non-empty customHost) adds a TLS block covering customHost;
// the sslip.io host is always served over plain HTTP. Called with
// customHost="", tlsSecret="" this reproduces LuncurObjects' original
// single-rule Ingress byte-for-byte.
func PanelIngress(externalIP, customHost, tlsSecret string) (render.Object, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":       "luncur",
		"app.kubernetes.io/managed-by": "luncur",
	}
	pathType := netv1.PathTypePrefix
	rule := func(host string) netv1.IngressRule {
		return netv1.IngressRule{
			Host: host,
			IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{
				Paths: []netv1.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
						Name: "luncur", Port: netv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}
	}

	rules := []netv1.IngressRule{rule(PanelHost(externalIP))}
	if customHost != "" {
		rules = append(rules, rule(customHost))
	}
	spec := netv1.IngressSpec{Rules: rules}
	if tlsSecret != "" && customHost != "" {
		spec.TLS = []netv1.IngressTLS{{Hosts: []string{customHost}, SecretName: tlsSecret}}
	}

	ing := &netv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Name: "luncur", Namespace: systemNamespace, Labels: labels},
		Spec:       spec,
	}
	b, err := json.Marshal(ing)
	if err != nil {
		return render.Object{}, err
	}
	return render.Object{Kind: "Ingress", JSON: b}, nil
}

// ForwardIngressName is deterministic so destroy paths can compute it
// without a rendered manifest at hand.
func ForwardIngressName(app, namespace string) string {
	return "fwd-" + app + "-" + namespace
}

// ForwardIngress routes one internal app's forward host to luncur itself;
// the server's host check proxies it onward after cookie auth. No TLS
// block: forward hosts share the sslip.io plain-HTTP posture for now.
func ForwardIngress(host, app, namespace string) (render.Object, error) {
	labels := map[string]string{
		"app.kubernetes.io/name":       "luncur",
		"app.kubernetes.io/managed-by": "luncur",
		"luncur.dev/forward":           "true",
	}
	pathType := netv1.PathTypePrefix
	ing := &netv1.Ingress{
		TypeMeta:   metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Name: ForwardIngressName(app, namespace), Namespace: systemNamespace, Labels: labels},
		Spec: netv1.IngressSpec{Rules: []netv1.IngressRule{{
			Host: host,
			IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{
				Paths: []netv1.HTTPIngressPath{{
					Path: "/", PathType: &pathType,
					Backend: netv1.IngressBackend{Service: &netv1.IngressServiceBackend{
						Name: "luncur", Port: netv1.ServiceBackendPort{Number: 80},
					}},
				}},
			}},
		}}},
	}
	b, err := json.Marshal(ing)
	if err != nil {
		return render.Object{}, err
	}
	return render.Object{Kind: "Ingress", JSON: b}, nil
}
