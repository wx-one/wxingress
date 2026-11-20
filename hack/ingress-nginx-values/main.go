// Command ingress-nginx-values turns the values of an ingress-nginx Helm
// release into values for charts/wxingress, for running wxingress in its
// place (docs/migration/ingress-nginx.md).
//
//	helm get values -n ingress-nginx ingress-nginx -o yaml | go run ./hack/ingress-nginx-values > wxingress.yaml
//	kubectl -n kube-system get helmchartconfig rke2-ingress-nginx -o yaml | go run ./hack/ingress-nginx-values -rke2 > wxingress.yaml
//
// The input is the values (as `helm get values` prints them) or an RKE2
// HelmChartConfig, whose spec.valuesContent are the values. With -rke2 the
// defaults RKE2 gives its rke2-ingress-nginx come first (a DaemonSet with
// hostPort 80/443, ClusterFirstWithHostNet, Ingresses without a class
// served). The output is YAML for `helm install -f`; what has no counterpart
// or needs a look goes to stderr, one line each, beginning with "warning:".
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wx-one/wxingress/internal/configs"
	"sigs.k8s.io/yaml"
)

func main() {
	rke2 := flag.Bool("rke2", false, "start from the defaults of RKE2's rke2-ingress-nginx")
	flag.Parse()

	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, warnings, err := Translate(in, *rke2)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
	os.Stdout.Write(out)
}

type values = map[string]interface{}

// rke2Defaults are the values RKE2 sets for rke2-ingress-nginx beyond the
// upstream chart's defaults.
func rke2Defaults() values {
	return values{"controller": values{
		"kind":                     "DaemonSet",
		"dnsPolicy":                "ClusterFirstWithHostNet",
		"hostPort":                 values{"enabled": true, "ports": values{"http": 80, "https": 443}},
		"watchIngressWithoutClass": true,
		"publishService":           values{"enabled": false},
		"service":                  values{"enabled": false},
		"metrics":                  values{"enabled": true, "port": 10254},
		"ingressClassResource":     values{"name": "nginx", "enabled": true, "controllerValue": "k8s.io/ingress-nginx"},
		// the chart's own defaults of rke2-ingress-nginx 4.12.600: its webhook's certificate made with Rancher's mirror of kube-webhook-certgen
		"admissionWebhooks": values{"enabled": true, "failurePolicy": "Fail", "patch": values{"image": values{
			"repository": "rancher/mirrored-ingress-nginx-kube-webhook-certgen", "tag": "v1.6.2",
		}}},
	}}
}

// Translate is the work of main: ingress-nginx values in, wxingress values
// and warnings out.
func Translate(in []byte, rke2 bool) ([]byte, []string, error) {
	var doc values
	if err := yaml.Unmarshal(in, &doc); err != nil {
		return nil, nil, fmt.Errorf("the input is not YAML: %w", err)
	}
	if doc["kind"] == "HelmChartConfig" {
		spec, _ := doc["spec"].(values)
		content, _ := spec["valuesContent"].(string)
		doc = values{}
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
			return nil, nil, fmt.Errorf("spec.valuesContent is not YAML: %w", err)
		}
	}
	if doc == nil {
		doc = values{}
	}
	if rke2 {
		doc = merge(rke2Defaults(), doc)
	}
	t := &translator{out: values{}}
	t.translate(doc)
	sort.Strings(t.warnings)
	b, err := yaml.Marshal(t.out)
	if err != nil {
		return nil, nil, err
	}
	return b, t.warnings, nil
}

// merge lays b over a, maps key by key.
func merge(a, b values) values {
	out := values{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		am, aok := out[k].(values)
		bm, bok := v.(values)
		if aok && bok {
			out[k] = merge(am, bm)
		} else {
			out[k] = v
		}
	}
	return out
}

type translator struct {
	out      values
	warnings []string
}

func (t *translator) warn(format string, args ...interface{}) {
	t.warnings = append(t.warnings, fmt.Sprintf(format, args...))
}

// set puts v at the dotted path of the output.
func (t *translator) set(path string, v interface{}) {
	keys := strings.Split(path, ".")
	m := t.out
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(values)
		if !ok {
			next = values{}
			m[k] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = v
}

func (t *translator) get(path string) interface{} {
	var cur interface{} = t.out
	for _, k := range strings.Split(path, ".") {
		m, ok := cur.(values)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

// same keys, other place
var moved = map[string]string{
	"replicaCount":                  "controller.replicaCount",
	"hostNetwork":                   "controller.hostNetwork",
	"dnsPolicy":                     "controller.dnsPolicy",
	"nodeSelector":                  "controller.nodeSelector",
	"tolerations":                   "controller.tolerations",
	"affinity":                      "controller.affinity",
	"topologySpreadConstraints":     "controller.topologySpreadConstraints",
	"resources":                     "controller.resources",
	"priorityClassName":             "controller.priorityClassName",
	"terminationGracePeriodSeconds": "controller.terminationGracePeriodSeconds",
	"minReadySeconds":               "controller.minReadySeconds",
	"podAnnotations":                "controller.pod.annotations",
	"podLabels":                     "controller.pod.extraLabels",
	"updateStrategy":                "controller.strategy",
	"lifecycle":                     "controller.lifecycle",
	"extraEnvs":                     "controller.env",
	"extraVolumes":                  "controller.volumes",
	"extraVolumeMounts":             "controller.volumeMounts",
	"extraInitContainers":           "controller.initContainers",
	"extraContainers":               "controller.extraContainers",
	"podSecurityContext":            "controller.podSecurityContext",
	"watchIngressWithoutClass":      "controller.watchIngressWithoutClass",
	"allowSnippetAnnotations":       "controller.enableSnippets",
	"electionID":                    "controller.reportIngressStatus.leaderElectionLockName",
	"autoscaling":                   "controller.autoscaling",
}

// nothing to carry over, said once
var dropped = map[string]string{
	"image":                       "the image is wxingress's",
	"containerName":               "",
	"containerPort":               "wxingress listens on 80 and 443 (controller.containerPort)",
	"opentelemetry":               "no OpenTelemetry module",
	"maxmindLicenseKey":           "no GeoIP2 download",
	"addHeaders":                  "put proxy-set-headers/add-headers into the ConfigMap of wxingress",
	"proxySetHeaders":             "put proxy-set-headers into the ConfigMap of wxingress",
	"keda":                        "",
	"customTemplate":              "wxingress has its own templates",
	"enableAnnotationValidations": "",
	"enableTopologyAwareRouting":  "controller.enableTopologyAwareRouting",
	"healthCheckPath":             "",
	"healthCheckHost":             "",
	"livenessProbe":               "wxingress has its own probes (controller.readyStatus)",
	"readinessProbe":              "wxingress has its own probes (controller.readyStatus)",
	"startupProbe":                "",
	"sysctls":                     "",
	"labels":                      "",
	"annotations":                 "",
	"scope":                       "use controller.watchNamespace",
	"configMapNamespace":          "",
	"tcp":                         "controller.tcp.annotations and configMapNamespace: the chart's ConfigMap of controller.tcpServices is in its namespace",
	"udp":                         "controller.udp.annotations and configMapNamespace: the chart's ConfigMap of controller.udpServices is in its namespace",
	"reportNodeInternalIp":        "",
	"enableMimalloc":              "",
	"shareProcessNamespace":       "",
	"dnsConfig":                   "",
	"hostAliases":                 "",
	"hostname":                    "",
	"existingPsp":                 "",
	"networkPolicy":               "",
	"progressDeadlineSeconds":     "",
	"runtimeClassName":            "",
	"unhealthyPodEvictionPolicy":  "",
	"disableLeaderElection":       "",
	"terminateTLS":                "",
}

func (t *translator) translate(doc values) {
	t.set("controller.ingressNginxCompat", true)
	c, _ := doc["controller"].(values)

	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := c[k]
		switch k {
		case "kind":
			kind := strings.ToLower(fmt.Sprint(v))
			if kind == "both" {
				t.warn("controller.kind Both: wxingress runs one kind, daemonset taken")
				kind = "daemonset"
			}
			t.set("controller.kind", kind)
		case "hostPort":
			hp, _ := v.(values)
			t.set("controller.hostPort.enable", hp["enabled"] == true)
			if p, ok := hp["ports"].(values); ok {
				if p["http"] != nil {
					t.set("controller.hostPort.http", p["http"])
				}
				if p["https"] != nil {
					t.set("controller.hostPort.https", p["https"])
				}
			}
		case "ingressClassResource":
			r, _ := v.(values)
			if r["name"] != nil {
				t.set("controller.ingressClass.name", r["name"])
			}
			if r["controllerValue"] != nil {
				t.set("controller.ingressClass.controllers", []interface{}{r["controllerValue"]})
			}
			if r["enabled"] != nil {
				t.warn("controller.ingressClassResource.enabled: the IngressClass belongs to the ingress-nginx release; controller.ingressClass.create=false keeps wxingress from owning it - set it true after the ingress-nginx release is gone")
			}
			t.set("controller.ingressClass.create", false)
			if r["default"] != nil {
				t.set("controller.ingressClass.setAsDefaultIngress", r["default"])
			}
			if r["parameters"] != nil {
				t.warn("controller.ingressClassResource.parameters: IngressClass parameters are not used")
			}
		case "ingressClass":
			if t.get("controller.ingressClass.name") == nil {
				t.set("controller.ingressClass.name", v)
			}
		case "ingressClassByName":
			if v == true {
				t.warn("controller.ingressClassByName: wxingress serves the class of -ingress-class by name anyway")
			}
		case "config":
			cfg, _ := v.(values)
			data := map[string]string{}
			for ck, cv := range cfg {
				data[ck] = fmt.Sprint(cv)
			}
			// the keys of wxingress, as the controller translates them with -ingress-nginx-defaults
			entries, warnings, unsafe := configs.TranslateIngressNginxConfigMap(data)
			for _, w := range warnings {
				t.warn("controller.config: %s", w)
			}
			if len(unsafe) > 0 {
				t.warn("controller.config: UNSAFE - %s restrict access in ingress-nginx and are not supported", strings.Join(unsafe, ", "))
			}
			if len(entries) > 0 {
				out := values{}
				for ek, ev := range entries {
					out[ek] = ev
				}
				t.set("controller.config.entries", out)
			}
		case "service":
			t.service(v)
		case "publishService":
			ps, _ := v.(values)
			if ps["enabled"] == false {
				t.set("controller.reportIngressStatus.enable", false)
			} else if ps["pathOverride"] != nil {
				t.set("controller.reportIngressStatus.externalService", ps["pathOverride"])
			}
		case "metrics":
			t.metrics(v)
		case "extraArgs":
			t.extraArgs(v)
		case "minAvailable", "maxUnavailable":
			t.set("controller.podDisruptionBudget.enabled", true)
			t.set("controller.podDisruptionBudget."+k, v)
		case "admissionWebhooks":
			t.admissionWebhooks(v)
		case "allowSnippetAnnotations":
			t.set(moved[k], v)
			if v == true {
				t.warn("controller.allowSnippetAnnotations: snippets are nginx configuration of the Ingress authors; wxingress's snippet support is in docs/user-guide/ingress-nginx-compatibility.md")
			}
		default:
			if dst, ok := moved[k]; ok {
				if k == "updateStrategy" {
					if m, ok := v.(values); ok && len(m) == 0 {
						continue
					}
				}
				t.set(dst, v)
			} else if why, ok := dropped[k]; ok {
				if why != "" && !empty(v) {
					t.warn("controller.%s: %s", k, why)
				}
			} else if !empty(v) {
				t.warn("controller.%s: no counterpart, not carried over", k)
			}
		}
	}

	if db, ok := doc["defaultBackend"].(values); ok && db["enabled"] == true {
		t.warn("defaultBackend: the chart of wxingress deploys no default backend; run it (its Service is <release>-defaultbackend of the ingress-nginx release, which goes with it) and set controller.defaultBackendService=<namespace>/<service>")
	}
	for _, proto := range []string{"tcp", "udp"} {
		if v, ok := doc[proto].(values); ok && len(v) > 0 {
			services := values{}
			for port, target := range v {
				services[port] = fmt.Sprint(target)
			}
			t.set("controller."+proto+"Services", services)
		}
	}
	for _, k := range []string{"imagePullSecrets", "rbac", "serviceAccount", "podSecurityPolicy", "commonLabels", "namespaceOverride", "nameOverride", "fullnameOverride", "revisionHistoryLimit", "portNamePrefix", "dhParam", "global"} {
		if v, ok := doc[k]; ok && !empty(v) {
			switch k {
			case "imagePullSecrets":
				var names []interface{}
				list, _ := v.([]interface{})
				for _, s := range list {
					if m, ok := s.(values); ok && m["name"] != nil {
						names = append(names, m["name"])
					}
				}
				t.set("controller.serviceAccount.imagePullSecretsNames", names)
			case "commonLabels":
				t.set("commonLabels", v)
			case "rbac", "serviceAccount", "podSecurityPolicy":
				t.warn("%s: wxingress's chart makes its own RBAC and ServiceAccount", k)
			default:
				t.warn("%s: not carried over", k)
			}
		}
	}
}

// admissionWebhookKeys are the keys of ingress-nginx's controller.admissionWebhooks that charts/wxingress has under
// the same names, with those of their maps it has (nil: all of it).
var admissionWebhookKeys = map[string][]string{
	"enabled": nil, "name": nil, "annotations": nil, "extraEnvs": nil, "failurePolicy": nil, "timeoutSeconds": nil,
	"port": nil, "certificate": nil, "key": nil, "namespaceSelector": nil, "objectSelector": nil, "labels": nil,
	"service":         {"annotations", "externalIPs", "loadBalancerSourceRanges", "servicePort", "type", "clusterIP", "loadBalancerIP"},
	"createSecretJob": {"name", "activeDeadlineSeconds", "securityContext", "resources", "volumeMounts", "volumes"},
	"patchWebhookJob": {"name", "activeDeadlineSeconds", "securityContext", "resources", "volumeMounts", "volumes"},
	"patch": {"enabled", "image", "priorityClassName", "runtimeClassName", "podAnnotations", "networkPolicy", "nodeSelector",
		"tolerations", "labels", "securityContext", "rbac", "serviceAccount"},
	"certManager": {"enabled", "rootCert", "admissionCert", "issuerRef"},
}

// admissionWebhooks carries ingress-nginx's admission webhook over to wxingress's, which has its values.
func (t *translator) admissionWebhooks(v interface{}) {
	aw, _ := v.(values)
	keys := make([]string, 0, len(aw))
	for k := range aw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sub, known := admissionWebhookKeys[k]
		if !known {
			if !empty(aw[k]) {
				t.warn("controller.admissionWebhooks.%s: no counterpart, not carried over", k)
			}
			continue
		}
		m, isMap := aw[k].(values)
		if sub == nil || !isMap {
			t.set("controller.admissionWebhooks."+k, aw[k])
			continue
		}
		subKeys := make([]string, 0, len(m))
		for sk := range m {
			subKeys = append(subKeys, sk)
		}
		sort.Strings(subKeys)
		for _, sk := range subKeys {
			if !contains(sub, sk) {
				if !empty(m[sk]) {
					t.warn("controller.admissionWebhooks.%s.%s: no counterpart, not carried over", k, sk)
				}
				continue
			}
			t.set("controller.admissionWebhooks."+k+"."+sk, m[sk])
		}
	}
	// an image of its own: not with the digest of wxingress's default one
	if img, ok := t.get("controller.admissionWebhooks.patch.image").(values); ok && img["digest"] == nil && (img["repository"] != nil || img["tag"] != nil || img["image"] != nil) {
		t.set("controller.admissionWebhooks.patch.image.digest", "")
	}
	if aw["enabled"] == true {
		policy := aw["failurePolicy"]
		if policy == nil {
			policy = "Fail"
		}
		t.warn("controller.admissionWebhooks: wxingress's webhook takes ingress-nginx's place (failurePolicy %v); with Fail no Ingress can be created or changed while no wxingress pod is Ready - where its pods come node by node (hostPort), install with failurePolicy Ignore and make it Fail when ingress-nginx's webhook goes, as docs/migration/rke2.md does", policy)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (t *translator) service(v interface{}) {
	s, _ := v.(values)
	if s["enabled"] == false {
		t.set("controller.service.create", false)
	}
	for src, dst := range map[string]string{
		"type": "type", "annotations": "annotations", "labels": "extraLabels",
		"loadBalancerIP": "loadBalancerIP", "loadBalancerSourceRanges": "loadBalancerSourceRanges",
		"externalTrafficPolicy": "externalTrafficPolicy", "externalIPs": "externalIPs",
		"clusterIP": "clusterIP", "ipFamilyPolicy": "ipFamilyPolicy", "ipFamilies": "ipFamilies",
		"loadBalancerClass": "loadBalancerClass",
	} {
		if sv, ok := s[src]; ok && !empty(sv) {
			t.set("controller.service."+dst, sv)
		}
	}
	if np, ok := s["nodePorts"].(values); ok {
		if np["http"] != nil && np["http"] != "" {
			t.set("controller.service.httpPort.nodePort", np["http"])
		}
		if np["https"] != nil && np["https"] != "" {
			t.set("controller.service.httpsPort.nodePort", np["https"])
		}
	}
	if s["internal"] != nil {
		if in, ok := s["internal"].(values); ok && in["enabled"] == true {
			t.warn("controller.service.internal: a second, internal Service is not made; use controller.service.customPorts or a Service of your own")
		}
	}
}

func (t *translator) metrics(v interface{}) {
	m, _ := v.(values)
	if m["enabled"] == true {
		t.set("prometheus.create", true)
		port := m["port"]
		if port == nil {
			port = 10254
		}
		t.set("prometheus.port", port)
		t.warn("controller.metrics: wxingress's metrics have their own names until drop-in step 6 (ingress-nginx compatible metrics)")
	} else if m["enabled"] == false {
		t.set("prometheus.create", false)
	}
	if svc, ok := m["service"].(values); ok && svc["enabled"] == true {
		t.set("prometheus.service.create", true)
	}
	if sm, ok := m["serviceMonitor"].(values); ok && sm["enabled"] == true {
		t.set("prometheus.service.create", true)
		t.set("prometheus.serviceMonitor.create", true)
		if l, ok := sm["additionalLabels"]; ok && !empty(l) {
			t.set("prometheus.serviceMonitor.labels", l)
		}
	}
	if pr, ok := m["prometheusRule"].(values); ok && pr["enabled"] == true {
		t.warn("controller.metrics.prometheusRule: rules on ingress-nginx's metric names, not carried over")
	}
}

func (t *translator) extraArgs(v interface{}) {
	args, _ := v.(values)
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		a := fmt.Sprint(args[k])
		switch k {
		case "controller-class":
			t.set("controller.ingressClass.controllers", []interface{}{a})
		case "ingress-class":
			t.set("controller.ingressClass.name", a)
		case "watch-ingress-without-class":
			t.set("controller.watchIngressWithoutClass", a == "true" || a == "")
		case "watch-namespace":
			t.set("controller.watchNamespace", a)
		case "publish-status-address":
			t.set("controller.config.entries.external-status-address", a)
		case "update-status":
			t.set("controller.reportIngressStatus.enable", a == "true")
		case "enable-ssl-passthrough":
			t.set("controller.enableTLSPassthrough", a == "true" || a == "")
		case "default-ssl-certificate":
			t.warn("extraArgs.default-ssl-certificate %s: set controller.defaultTLS.secret to it", a)
		case "default-backend-service":
			t.set("controller.defaultBackendService", a)
		case "tcp-services-configmap":
			t.set("controller.tcpServicesConfigMap", a)
		case "udp-services-configmap":
			t.set("controller.udpServicesConfigMap", a)
		default:
			t.warn("extraArgs.%s: no counterpart, not carried over", k)
		}
	}
}

func empty(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case values:
		return len(x) == 0
	case []interface{}:
		return len(x) == 0
	case bool:
		return !x
	}
	return false
}
