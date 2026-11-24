# RKE2: node by node

RKE2 bundles ingress-nginx as `rke2-ingress-nginx`: a DaemonSet that holds ports 80 and 443 of every node.
This runbook replaces it with wxingress one node at a time, so that every node can go back at any moment
and no request is lost. It was used on production RKE2 clusters with 1 to 21 ingress nodes. The same
approach works for any ingress-nginx DaemonSet with hostPort or hostNetwork, including RKE1's ingress
addon (patch the DaemonSet instead of the HelmChartConfig).

The example values are in
[`deployments/rke2-cutover/`](https://github.com/wx-one/wxingress/tree/main/deployments/rke2-cutover).

!!! tip "Lessons from production switches"
    - **Probe before the first node.** Install a second, invisible wxingress release (a Deployment without
      hostPort, admission webhook, Service or status) and send the probe of step 0 to its pod address. It
      shows every difference before a single request is affected.
    - **Wait before you probe.** Give a pod half a minute after it becomes Ready before you compare its
      answers.
    - **Set `worker-shutdown-timeout: 10s`** in both ConfigMaps. With ingress-nginx's 240 s, a pod that is
      stopped holds the node's ports for minutes while WebSockets drain.
    - **Never use `--force`** to delete a pod on a hostPort node. The old pod's port forwarding rule stays in
      the node's nat table, ahead of the new one, and the node answers nothing.
    - **Check taints, NetworkPolicies and the admission webhook** (step 0 and step 5) before the switch,
      not after.
    - **Half the nodes at once** is fine once the first node went well: label several nodes, each with its
      own probe and automatic way back.


The cluster: RKE2 1.31 with its bundled `rke2-ingress-nginx` (ingress-nginx
1.12, Helm chart `rke2-ingress-nginx` in `kube-system`, configured through
the HelmChartConfig `rke2-ingress-nginx`) as a DaemonSet with hostPort 80/443
on every Linux node that tolerates the taints `app`, `customer` and `gitlab`.
Traffic reaches the nodes directly (DNS or an outside balancer to the node
addresses), not through a Service.

Two controllers cannot hold hostPort 80 on the same node. So every node runs
exactly one of them, chosen by a node label:

    wx1.eu/ingress=wxingress   wxingress runs here, rke2-ingress-nginx does not
    (no label)                 rke2-ingress-nginx, as before

Both serve the same Ingresses (wxingress takes the IngressClass `nginx` of
controller `k8s.io/ingress-nginx` and those without a class), so a node can go
over and back at any time; a request that lands on a node gets the same answer
from either controller, apart from the differences in [ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md).

Commands are for a shell with `KUBECONFIG` of the cluster and the wxingress
checkout as the working directory. `<node>` is a node name, `<tag>` the
wxingress image tag, `<ip>` the node's InternalIP.

## 0. Before

- Read what the Ingresses use that wxingress does not do the same way:

      kubectl get ingress -A -o json \
        | jq -r '.items[].metadata.annotations // {} | keys[] | select(startswith("nginx.ingress.kubernetes.io/"))' \
        | sort | uniq -c

  and compare with the table in [ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md); wxingress puts a
  Warning event on every Ingress with an annotation it does not support. The
  Ingresses of this cluster, anonymised, are the corpus
  `dataplane/e2e/corpus/rke2-cluster/` (regenerate it with
  `dataplane/e2e/corpus/anonymise.py`); `dataplane/e2e/ingress-nginx-parity.sh`
  (drop-in step 7) runs it against ingress-nginx and wxingress on kind - run
  it against the tag you install.
- Write down the current state, for the way back:

      kubectl -n kube-system get helmchartconfig rke2-ingress-nginx -o yaml > /tmp/hcc-before.yaml
      kubectl -n kube-system get ds rke2-ingress-nginx-controller -o yaml > /tmp/ds-before.yaml
      kubectl get ingressclass nginx -o yaml > /tmp/ingressclass-before.yaml
      kubectl get validatingwebhookconfiguration rke2-ingress-nginx-admission -o yaml > /tmp/webhook-before.yaml
      kubectl get ingress -A -o wide > /tmp/ingress-before.txt

- For every host, a probe that you will repeat on each node after its switch:

      kubectl get ingress -A -o jsonpath='{range .items[*]}{range .spec.rules[*]}{.host}{"\n"}{end}{end}' \
        | sort -u | grep -v '^\*' > /tmp/hosts.txt

      probe() { # probe <ip>: status and the first bytes of the answer of each host
        while read -r h; do
          printf '%s ' "$h"
          curl -sk -o /dev/null -w '%{http_code} %{size_download}\n' --max-time 10 \
            --resolve "$h:443:$1" --resolve "$h:80:$1" "https://$h/"
        done < /tmp/hosts.txt
      }
      probe <ip of a node not yet switched> > /tmp/probe-ingress-nginx.txt

## 1. rke2-ingress-nginx keeps off labelled nodes

RKE2's helm-controller applies the HelmChartConfig. Add the affinity to the
values it has (`controller.tolerations` and `controller.config` stay):

    kubectl -n kube-system patch helmchartconfig rke2-ingress-nginx --type merge -p "$(cat <<'EOF'
    spec:
      valuesContent: |-
        controller:
          tolerations:
            - key: app
              operator: Exists
          config:
            strict-validate-path-type: "false"
          updateStrategy:
            type: OnDelete
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                  - matchExpressions:
                      - key: wx1.eu/ingress
                        operator: NotIn
                        values: [wxingress]
    EOF
    )"

([`deployments/rke2-cutover/rke2-step1-values.yaml`](https://github.com/wx-one/wxingress/blob/main/deployments/rke2-cutover/rke2-step1-values.yaml) is what it adds.) The affinity
changes the pod template; with the chart's RollingUpdate that would restart
every rke2-ingress-nginx pod, a node at a time, and each node would refuse
connections while its pod restarts - the rehearsal on kind
(`dataplane/e2e/ingress-nginx-parity.sh`) saw those failures. `OnDelete` in
the same change makes the DaemonSet replace no running pod: a pod goes only
when its node no longer fits the affinity, in step 3, after the node is out
of the balancer. Wait for helm-controller and check:

    kubectl -n kube-system get job -l helmcharts.helm.cattle.io/chart=rke2-ingress-nginx
    kubectl -n kube-system get ds rke2-ingress-nginx-controller \
      -o jsonpath='{.spec.updateStrategy.type} {.spec.template.spec.affinity}{"\n"}'   # OnDelete {...NotIn...}
    kubectl -n kube-system get pod -l app.kubernetes.io/name=rke2-ingress-nginx   # the same pods, the same ages

No node has the label yet, so nothing else changes.

## 2. Install wxingress (no pod yet)

    helm upgrade --install wxingress charts/wxingress -n wxingress --create-namespace \
      -f deployments/rke2-cutover/values.yaml --set controller.image.tag=<tag> \
      --set controller.admissionWebhooks.failurePolicy=Ignore
    kubectl -n wxingress get ds wxingress-controller    # DESIRED 0
    kubectl get validatingwebhookconfiguration wxingress-controller-admission \
      -o jsonpath='{.webhooks[0].failurePolicy}{"\n"}'   # Ignore

The DaemonSet runs only on labelled nodes, writes no Ingress status, and does
not touch the IngressClass `nginx`. Its admission webhook
(`wxingress-controller-admission`, served by the wxingress pods - the jobs of
the chart made its certificate) checks every Ingress written beside
rke2-ingress-nginx's from the first switched node on; `Ignore` lets an
Ingress through while no wxingress pod answers, which with `Fail` - the
value of `deployments/rke2-cutover/values.yaml` - would refuse every Ingress written until step 3.
Step 5 makes it `Fail`. That is a change of the ValidatingWebhookConfiguration
alone: no pod restarts for it.

## 3. The first node

Take a node with little traffic first. Take it out of the balancer (or the
DNS) in front of the nodes and wait until it has no more connections -
the node refuses connections from the moment its rke2-ingress-nginx pod goes
until the wxingress pod listens, some seconds. Then switch it:

    kubectl label node <node> wx1.eu/ingress=wxingress

The DaemonSet controller removes the rke2-ingress-nginx pod of the node (it
waits for open connections, up to 300 s, `/wait-shutdown`); the wxingress pod
is scheduled as soon as hostPort 80/443 are free:

    kubectl -n kube-system get pod -l app.kubernetes.io/name=rke2-ingress-nginx \
      --field-selector spec.nodeName=<node> -w            # until it is gone
    kubectl -n wxingress get pod -o wide --field-selector spec.nodeName=<node> -w   # until 1/1 Running

Check it:

    kubectl -n wxingress logs "$(kubectl -n wxingress get pod -o name --field-selector spec.nodeName=<node>)" \
      --tail=50 | grep -iE 'error|warn'     # this node's pod; logs ds/... picks any of them
    probe <ip> > /tmp/probe-<node>.txt
    diff /tmp/probe-ingress-nginx.txt /tmp/probe-<node>.txt

Same status codes for every host; differences in the size are fine where
pages differ per request. Put the node back into the balancer, and watch its
traffic (error rate, latency) for a while.

### The way back for one node

As the switch: the node out of the balancer first, until it has no more
connections - the node refuses connections from the moment the wxingress pod
goes until rke2-ingress-nginx listens again - then

    kubectl label node <node> wx1.eu/ingress-
    kubectl -n kube-system get pod -l app.kubernetes.io/name=rke2-ingress-nginx \
      --field-selector spec.nodeName=<node> -w            # until 1/1 Running
    probe <ip> > /tmp/probe-<node>-back.txt
    diff /tmp/probe-ingress-nginx.txt /tmp/probe-<node>-back.txt

and back into the balancer. The wxingress pod goes (preStop sleep 5 s, then
nginx finishes its requests); the DaemonSet of rke2-ingress-nginx, OnDelete
since step 1, makes a pod for the node again, with its template of now. While
step 5 has not run, nothing else is involved. After step 5 the new pod does not
serve rke2-ingress-nginx's webhook (it is off) and wxingress's, with
`failurePolicy: Fail`, is the only one: keep at least one node on wxingress,
or go the whole way back ("The way back, all of it"), which turns the webhooks
round first.

## 4. The other nodes, one by one

For each node, after it is out of the balancer and has no more connections:

    switch() { # switch <node>: labelled, its wxingress pod Ready, its probe against ingress-nginx's
      kubectl label node "$1" wx1.eu/ingress=wxingress
      until kubectl -n wxingress get pod -o name --field-selector spec.nodeName="$1" | grep -q .; do sleep 2; done
      kubectl -n wxingress wait pod -l app.kubernetes.io/instance=wxingress \
        --field-selector spec.nodeName="$1" --for=condition=Ready --timeout=10m || return 1
      probe "$(kubectl get node "$1" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')" \
        > "/tmp/probe-$1.txt"
      diff <(cut -d' ' -f1,2 /tmp/probe-ingress-nginx.txt) <(cut -d' ' -f1,2 "/tmp/probe-$1.txt")   # host and status
    }
    switch <node>

then back into the balancer, and the next node. Not as a loop over the
nodes: each one leaves the balancer before its label and returns after its
probe. The diff compares the status codes; sizes differ where pages differ
per request. Stop before the last node that still runs rke2-ingress-nginx
and go to step 5.

    kubectl get nodes -L wx1.eu/ingress
    kubectl -n kube-system get pod -l app.kubernetes.io/name=rke2-ingress-nginx -o wide

## 5. Before the last rke2-ingress-nginx pod: the admission webhook

`rke2-ingress-nginx-admission` validates every Ingress create and update with
`failurePolicy: Fail` against the ingress-nginx pods. With none left, every
`kubectl apply` of an Ingress fails cluster-wide (Helm releases, cert-manager's
http01 solver Ingresses). wxingress's webhook takes its place: it refuses what
ingress-nginx's refuses - an Ingress whose host and path another Ingress has,
a snippet annotation while snippets are off - and what wxingress could not
serve: an invalid one, one whose configuration fails `nginx -t`
([ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md), "The admission webhook").

First wxingress's, with `failurePolicy: Fail`, now that the switched nodes
answer it:

    helm upgrade wxingress charts/wxingress -n wxingress --reuse-values \
      --set controller.admissionWebhooks.failurePolicy=Fail
    kubectl get validatingwebhookconfiguration wxingress-controller-admission \
      -o jsonpath='{.webhooks[0].failurePolicy}{"\n"}'   # Fail
    # every Ingress there is passes both webhooks, as written (without its resourceVersion: rke2-ingress-nginx
    # rewrites the status of every Ingress whenever one of its pods goes, and a stale one is a Conflict)
    kubectl get ingress -A -o json | jq 'del(.items[].metadata.resourceVersion)' \
      | kubectl apply --dry-run=server -f - 2>&1 | grep -c 'denied the request\|^Error'   # 0 (warnings are fine)
    # and wxingress's answers: an Ingress only it refuses (an invalid nginx.org annotation, a host of no one)
    kubectl get ingress -A -o json | jq '.items[0] | del(.spec.tls) | .spec.rules |= map(.host = "webhook-check.example")
        | .metadata = {name: "webhook-check", namespace: .metadata.namespace, annotations: {"nginx.org/proxy-connect-timeout": "soon"}}' \
      | kubectl apply --dry-run=server -f - 2>&1 | grep -c 'validate.wxingress.wizardtales.com'   # 1

Then rke2-ingress-nginx's goes, through the HelmChartConfig - deleting the
ValidatingWebhookConfiguration alone is undone by the next reconcile of the
chart - while one of its pods still answers:

    kubectl -n kube-system patch helmchartconfig rke2-ingress-nginx --type merge -p "$(cat <<'EOF'
    spec:
      valuesContent: |-
        controller:
          tolerations:
            - key: app
              operator: Exists
          config:
            strict-validate-path-type: "false"
          updateStrategy:
            type: OnDelete
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                  - matchExpressions:
                      - key: wx1.eu/ingress
                        operator: NotIn
                        values: [wxingress]
          admissionWebhooks:
            enabled: false
    EOF
    )"
    kubectl get validatingwebhookconfiguration rke2-ingress-nginx-admission   # NotFound

Then switch the last node as in step 3. From here on wxingress's webhook
alone checks the Ingresses; its counts are ingress-nginx's
`nginx_ingress_controller_check_success` and `_check_errors` (step 6).

## 6. Metrics

wxingress exports ingress-nginx's metrics - `nginx_ingress_controller_requests`,
the request histograms, the reloads, the certificates, the nginx processes,
with `controller_class="k8s.io/ingress-nginx"` - on 10254
([ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md), "Metrics"), so the ingress-nginx dashboards and
alerts show the switched nodes as they showed the others.

Rancher monitoring scrapes rke2-ingress-nginx through the Service
`pushprox-ingress-nginx-client` in `kube-system` (ServiceMonitor
`rancher-monitoring-ingress-nginx`), which selects the pods by
`app.kubernetes.io/name: rke2-ingress-nginx`. That key is the one the
wxingress chart selects its own pods by, so they cannot carry Rancher's
value; `deployments/rke2-cutover/values.yaml` makes a metrics Service and a ServiceMonitor of
wxingress's own, with the job `ingress-nginx` (`jobLabel: component`) and
`release: rancher-monitoring`. Rancher's Prometheus takes every ServiceMonitor:

    kubectl get prometheus -A -o jsonpath='{range .items[*]}{.spec.serviceMonitorSelector}{.spec.serviceMonitorNamespaceSelector}{"\n"}{end}'   # {}{}
    kubectl -n wxingress get servicemonitor wxingress-controller

Rancher's ServiceMonitor adds the labels `cluster_id` and `cluster_name` to
every series; give wxingress's the same, from Rancher's own:

    kubectl -n kube-system get servicemonitor rancher-monitoring-ingress-nginx \
      -o jsonpath='{.spec.endpoints[0].metricRelabelings}' > /tmp/relabelings.json
    helm upgrade wxingress charts/wxingress -n wxingress --reuse-values \
      --set-json "prometheus.serviceMonitor.endpoints=[{\"port\":\"prometheus\",\"metricRelabelings\":$(cat /tmp/relabelings.json)}]"

Check that a switched node's pod is scraped and counts:

    kubectl -n wxingress port-forward ds/wxingress-controller 10254 &
    curl -s localhost:10254/metrics | grep -E '^nginx_ingress_controller_(requests|config_last_reload_successful|ssl_expire_time_seconds)' | head

After step 7 the Service `pushprox-ingress-nginx-client` and Rancher's
ServiceMonitor go with the release; wxingress's stay.

## 7. After the last node

    kubectl -n kube-system get pod -l app.kubernetes.io/name=rke2-ingress-nginx   # none

Ingress status: rke2-ingress-nginx wrote the addresses of the nodes it ran
on, and so does wxingress from 5.7.1-wx12 (with `ingressNginxCompat`): the
ExternalIP or else the InternalIP of every node one of its pods runs on,
following the pods as nodes come and go. Argo CD waits on an Ingress without
an address as Progressing, so turn it on:

    helm upgrade wxingress charts/wxingress -n wxingress --reuse-values \
      --set controller.reportIngressStatus.enable=true

An address of your own, such as the load balancer's in front of the nodes,
wins over the nodes' (and external-dns publishes what the status says):

    --set-string controller.config.entries.external-status-address=203.0.113.10

Before 5.7.1-wx12 wxingress wrote no address without that key.

`-report-ingress-status` is an argument of the pod, and the DaemonSet is
OnDelete: the upgrade restarts no pod, and none writes a status until it is
restarted. Restart them as "Later changes" below says - a node out of the
balancer, its pod deleted, Ready again, back in - one node after the other;
the first restarted pod writes the status:

    kubectl get ingress -A -o wide

Then, by the person running the cluster, on every server node, in
`/etc/rancher/rke2/config.yaml`:

    disable:
      - rke2-ingress-nginx

and `systemctl restart rke2-server`, one server at a time. RKE2 uninstalls the
release: the empty DaemonSet, its Services and the IngressClass `nginx`.
wxingress goes on serving `nginx` by name without the IngressClass object;
let the chart own a new one:

    helm upgrade wxingress charts/wxingress -n wxingress --reuse-values \
      --set controller.ingressClass.create=true --set controller.ingressClass.setAsDefaultIngress=false
    kubectl get ingressclass nginx -o jsonpath='{.spec.controller}{"\n"}'   # nginx.wx1.eu/ingress-controller

Keep the label and the affinity: they say which nodes run the ingress, and
taking the affinity away changes the pod template, which with RollingUpdate
restarts the wxingress pod of every node in turn - each node refusing
connections for the seconds its pod restarts, as step 1 says of
rke2-ingress-nginx. A node that joins later gets the label to run wxingress.

Later changes of wxingress that restart its pods (an image, most chart
values) go the same way as step 3: a node out of the balancer, its pod
deleted (`kubectl -n wxingress delete pod -l app.kubernetes.io/instance=wxingress
--field-selector spec.nodeName=<node>`), Ready again, back in - with
`controller.strategy.type=OnDelete` in the values, so that the upgrade
itself restarts nothing.

## The way back, all of it

Before step 7, in this order (`dataplane/e2e/ingress-nginx-parity.sh` with
`BACK=1` rehearses it under load):

1. The webhooks turned round: rke2-ingress-nginx's back with `Ignore`,
   wxingress's `Ignore`. Not by applying `/tmp/hcc-before.yaml`: that takes
   the affinity and `OnDelete` away, so RollingUpdate restarts every
   rke2-ingress-nginx pod under traffic (step 1) and the DaemonSet makes pods
   for the labelled nodes that wait for their hostPorts while wxingress still
   holds them. Patch the HelmChartConfig of step 5 instead, with

       admissionWebhooks:
         enabled: true
         failurePolicy: Ignore

   in place of `enabled: false`. The webhook's argument and certificate are
   part of the pod: an rke2-ingress-nginx pod serves it only once it is made
   anew (`OnDelete`), so `Fail` here would refuse every Ingress until then.
   Then

       helm upgrade wxingress charts/wxingress -n wxingress --reuse-values \
         --set controller.admissionWebhooks.failurePolicy=Ignore

2. Every node back as "The way back for one node" says - out of the
   balancer, the label removed, rke2-ingress-nginx Ready, the probe, back
   in - one after the other. Their new pods serve the webhook.
3. rke2-ingress-nginx's webhook `Fail` again: the HelmChartConfig of step 1
   (the step-1 values with no `admissionWebhooks`; the chart's default is
   `Fail`), and the check of step 5 with it:

       kubectl get validatingwebhookconfiguration rke2-ingress-nginx-admission \
         -o jsonpath='{.webhooks[0].failurePolicy}{"\n"}'   # Fail
       kubectl get ingress -A -o json | jq 'del(.items[].metadata.resourceVersion)' \
         | kubectl apply --dry-run=server -f - 2>&1 | grep -c 'denied the request\|^Error'   # 0

4. `helm uninstall wxingress -n wxingress`.

The affinity and `OnDelete` of step 1 stay: without the label the affinity
selects every node, and taking them out changes the pod template, which with
RollingUpdate restarts every pod in turn. Take them out later the same way
as a restart in "Later changes", node by node out of the balancer. As long as
`OnDelete` is set, an RKE2 upgrade does not restart rke2-ingress-nginx
either: its new image runs on a node once that node's pod is deleted.

After step 7: remove `rke2-ingress-nginx` from `disable` and restart
rke2-server, one server at a time; RKE2 installs the release again with the
HelmChartConfig as step 5 left it (its webhook off, the affinity) - on the
labelled nodes no pod - then the same as above.
