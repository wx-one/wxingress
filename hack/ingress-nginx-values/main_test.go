package main

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func translated(t *testing.T, in string, rke2 bool) (values, []string) {
	t.Helper()
	out, warnings, err := Translate([]byte(in), rke2)
	if err != nil {
		t.Fatal(err)
	}
	var v values
	if err := yaml.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v, warnings
}

func at(v values, path string) interface{} {
	var cur interface{} = v
	for _, k := range strings.Split(path, ".") {
		m, _ := cur.(values)
		cur = m[k]
	}
	return cur
}

func TestUpstreamValues(t *testing.T) {
	v, warnings := translated(t, `
controller:
  kind: Deployment
  replicaCount: 3
  ingressClassResource:
    name: public
    controllerValue: k8s.io/ingress-nginx-public
    enabled: true
    default: true
  config:
    use-forwarded-headers: true
    proxy-body-size: 10m
  service:
    type: LoadBalancer
    annotations: {a: b}
    externalTrafficPolicy: Local
  metrics:
    enabled: true
    serviceMonitor: {enabled: true, additionalLabels: {release: prom}}
  extraArgs:
    default-ssl-certificate: ns/cert
    publish-status-address: 192.0.2.1
    update-status: "false"
    default-backend-service: errors/pages
    udp-services-configmap: ingress-nginx/udp
  minAvailable: 1
  admissionWebhooks:
    enabled: true
    timeoutSeconds: 7
    namespaceSelector: {matchLabels: {team: a}}
    service: {servicePort: 443, ipFamilyPolicy: PreferDualStack}
    patch: {image: {registry: example.org, image: certgen, tag: v1}}
    certManager: {enabled: false}
  image: {tag: v1.12.6}
imagePullSecrets: [{name: pull}]
tcp: {"5432": "db/postgres:5432"}
defaultBackend: {enabled: true}
`, false)
	for path, want := range map[string]interface{}{
		"controller.kind":                                                 "deployment",
		"controller.replicaCount":                                         float64(3),
		"controller.ingressNginxCompat":                                   true,
		"controller.ingressClass.name":                                    "public",
		"controller.ingressClass.create":                                  false,
		"controller.ingressClass.setAsDefaultIngress":                     true,
		"controller.config.entries.use-forwarded-headers":                 "true",
		"controller.config.entries.client-max-body-size":                  "10m",
		"controller.config.entries.external-status-address":               "192.0.2.1",
		"controller.reportIngressStatus.enable":                           false,
		"controller.service.type":                                         "LoadBalancer",
		"controller.service.externalTrafficPolicy":                        "Local",
		"controller.podDisruptionBudget.minAvailable":                     float64(1),
		"prometheus.port":                                                 float64(10254),
		"prometheus.serviceMonitor.create":                                true,
		"prometheus.serviceMonitor.labels.release":                        "prom",
		"controller.tcpServices.5432":                                     "db/postgres:5432",
		"controller.defaultBackendService":                                "errors/pages",
		"controller.udpServicesConfigMap":                                 "ingress-nginx/udp",
		"controller.admissionWebhooks.enabled":                            true,
		"controller.admissionWebhooks.timeoutSeconds":                     float64(7),
		"controller.admissionWebhooks.namespaceSelector.matchLabels.team": "a",
		"controller.admissionWebhooks.service.servicePort":                float64(443),
		"controller.admissionWebhooks.patch.image.registry":               "example.org",
		"controller.admissionWebhooks.patch.image.digest":                 "",
		"controller.admissionWebhooks.certManager.enabled":                false,
	} {
		if got := at(v, path); got != want {
			t.Errorf("%s: got %v, wanted %v", path, got, want)
		}
	}
	if c := at(v, "controller.ingressClass.controllers").([]interface{}); len(c) != 1 || c[0] != "k8s.io/ingress-nginx-public" {
		t.Errorf("controllers: %v", c)
	}
	if s := at(v, "controller.serviceAccount.imagePullSecretsNames").([]interface{}); len(s) != 1 || s[0] != "pull" {
		t.Errorf("pull secrets: %v", s)
	}
	if at(v, "controller.image") != nil {
		t.Error("the image of ingress-nginx is carried over")
	}
	all := strings.Join(warnings, "\n")
	for _, w := range []string{"admissionWebhooks: wxingress's webhook takes", "admissionWebhooks.service.ipFamilyPolicy", "default-ssl-certificate", "defaultBackend:"} {
		if !strings.Contains(all, w) {
			t.Errorf("no warning about %s in\n%s", w, all)
		}
	}
}

func TestRKE2HelmChartConfig(t *testing.T) {
	v, _ := translated(t, `
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata: {name: rke2-ingress-nginx, namespace: kube-system}
spec:
  valuesContent: |-
    controller:
      tolerations: [{key: app, operator: Exists}]
      config:
        strict-validate-path-type: "false"
`, true)
	for path, want := range map[string]interface{}{
		"controller.kind":                                     "daemonset",
		"controller.hostPort.enable":                          true,
		"controller.hostPort.http":                            float64(80),
		"controller.dnsPolicy":                                "ClusterFirstWithHostNet",
		"controller.watchIngressWithoutClass":                 true,
		"controller.ingressClass.name":                        "nginx",
		"controller.ingressClass.create":                      false,
		"controller.service.create":                           false,
		"controller.reportIngressStatus.enable":               false,
		"prometheus.port":                                     float64(10254),
		"controller.admissionWebhooks.enabled":                true,
		"controller.admissionWebhooks.failurePolicy":          "Fail",
		"controller.admissionWebhooks.patch.image.repository": "rancher/mirrored-ingress-nginx-kube-webhook-certgen",
		"controller.admissionWebhooks.patch.image.digest":     "",
	} {
		if got := at(v, path); got != want {
			t.Errorf("%s: got %v, wanted %v", path, got, want)
		}
	}
	if c := at(v, "controller.ingressClass.controllers").([]interface{}); len(c) != 1 || c[0] != "k8s.io/ingress-nginx" {
		t.Errorf("controllers: %v", c)
	}
	if tol := at(v, "controller.tolerations").([]interface{}); len(tol) != 1 {
		t.Errorf("tolerations: %v", tol)
	}
}
