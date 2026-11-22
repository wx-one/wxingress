package k8s

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/configs"
	nl "github.com/wx-one/wxingress/internal/logger"
	networking "k8s.io/api/networking/v1"
)

// Without -ingress-nginx-cross-namespace, an Ingress of another namespace with the host of an Ingress loses the
// host instead of being merged into its server, with a warning that names nothing of the holder.
func TestIngressesOfAHostInOtherNamespacesAreNotMerged(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	now := time.Now()

	owner := createTestIngressWithPath("gitlab", "gitlab.example.com", "/", "webservice", now.Add(-time.Hour))
	sameNamespace := createTestIngressWithPath("gitlab-api", "gitlab.example.com", "/api", "api", now.Add(-time.Minute))
	intruder := createTestIngressWithPath("steal", "gitlab.example.com", "/login", "evil", now)
	intruder.Namespace = "tenant"

	c.AddOrUpdateIngress(owner)
	c.AddOrUpdateIngress(sameNamespace)
	_, problems := c.AddOrUpdateIngress(intruder)

	holder := holderOf(t, c, "gitlab.example.com")
	if holder.Ingress != owner || len(holder.Merged) != 1 || holder.Merged[0].Ingress != sameNamespace {
		t.Fatalf("holder %s with merged %v: want the owner with the Ingress of its namespace", holder.Ingress.Name, holder.Merged)
	}
	var found bool
	for _, p := range problems {
		if p.Object == intruder {
			found = true
			if p.Message != "All hosts are taken by other resources" {
				t.Errorf("problem of the intruder: %q", p.Message)
			}
		}
	}
	if !found {
		t.Errorf("problems %v: want one of the Ingress of the other namespace", problems)
	}
	// its Service is no upstream of the owner's server
	if resources := c.FindResourcesForService("tenant", "evil"); len(resources) != 0 {
		t.Errorf("resources of the intruder's Service: %v", resources)
	}
	intruderConfig := c.GetResourcesWithFilter(resourceFilter{Ingresses: true})
	for _, r := range intruderConfig {
		ic := r.(*IngressConfiguration)
		if ic.Ingress != intruder {
			continue
		}
		want := "host gitlab.example.com is taken by an Ingress of another namespace"
		if len(ic.Warnings) != 1 || ic.Warnings[0] != want {
			t.Errorf("warnings of the intruder %v, want %q", ic.Warnings, want)
		}
		for _, w := range ic.Warnings {
			if strings.Contains(w, owner.Name) || strings.Contains(w, owner.Namespace) {
				t.Errorf("the warning %q names the holder", w)
			}
		}
	}
}

// Without -ingress-nginx-cross-namespace, a canary attaches only to the Ingresses of its own namespace, with
// either way of attaching.
func TestCanariesOfOtherNamespacesDoNotAttach(t *testing.T) {
	t.Parallel()
	for _, perUpstream := range []bool{false, true} {
		c := createTestCanaryConfiguration()
		c.canaryPerUpstream = perUpstream
		now := time.Now()

		primary := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", now.Add(-time.Hour))
		c.AddOrUpdateIngress(primary)
		foreign := createTestCanaryIngress("divert", "cafe.example.com", "/tea", "evil", now, map[string]string{configs.CanaryWeightAnnotation: "100"})
		foreign.Namespace = "tenant"
		_, problems := c.AddOrUpdateIngress(foreign)

		if holder := holderOf(t, c, "cafe.example.com"); len(holder.Canaries) != 0 || len(holder.CanaryBackends) != 0 {
			t.Errorf("per upstream %v: canaries %v and backends %v, want none of another namespace", perUpstream, holder.Canaries, holder.CanaryBackends)
		}
		if len(problems) != 1 || problems[0].Object != foreign || !strings.HasSuffix(problems[0].Message, "in its namespace") {
			t.Errorf("per upstream %v: problems %+v", perUpstream, problems)
		}

		// one of its own namespace attaches
		own := createTestCanaryIngress("cafe-canary", "cafe.example.com", "/tea", "tea-canary", now.Add(time.Second), map[string]string{configs.CanaryWeightAnnotation: "10"})
		c.AddOrUpdateIngress(own)
		if holder := holderOf(t, c, "cafe.example.com"); len(holder.Canaries) != 1 || holder.Canaries[0] != own {
			t.Errorf("per upstream %v: canaries %v, want the one of the namespace", perUpstream, holder.Canaries)
		}

		// with it, as ingress-nginx, the oldest canary attaches, whatever its namespace
		c.crossNamespace = true
		c.rebuildHosts()
		if holder := holderOf(t, c, "cafe.example.com"); len(holder.Canaries) == 0 || holder.Canaries[0] != foreign {
			t.Errorf("per upstream %v, cross namespace: canaries %v, want the foreign one first", perUpstream, holder.Canaries)
		}
	}
}

// A canary's upstream is namespace, Service and port, not ingress-nginx's name of them, which is the same for
// namespace a-b and Service c as for namespace a and Service b-c.
func TestCanaryPerUpstreamDoesNotTakeAnUpstreamOfTheSameName(t *testing.T) {
	t.Parallel()
	c := createTestCanaryConfiguration()
	c.canaryPerUpstream = true
	c.crossNamespace = true
	now := time.Now()

	// a/b-c and a-b/c are both a-b-c-80 to ingress-nginx
	mine := createTestIngressWithPath("mine", "a.example.com", "/", "b-c", now.Add(-2*time.Hour))
	mine.Namespace = "a"
	theirs := createTestIngressWithPath("theirs", "b.example.com", "/", "c", now.Add(-time.Hour))
	theirs.Namespace = "a-b"
	canary := createTestCanaryIngress("mine-canary", "a.example.com", "/", "canary", now, map[string]string{configs.CanaryWeightAnnotation: "100"})
	canary.Namespace = "a"
	c.AddOrUpdateIngress(mine)
	c.AddOrUpdateIngress(theirs)
	c.AddOrUpdateIngress(canary)

	if holder := holderOf(t, c, "a.example.com"); len(holder.Canaries) != 1 {
		t.Errorf("canaries of the canary's own Ingress: %v", holder.Canaries)
	}
	if holder := holderOf(t, c, "b.example.com"); len(holder.Canaries) != 0 || len(holder.CanaryBackends) != 0 {
		t.Errorf("the Ingress of the other namespace has canaries %v, backends %v", holder.Canaries, holder.CanaryBackends)
	}
}

func withServerAlias(ing *networking.Ingress, aliases string) *networking.Ingress {
	ing.Annotations[configs.IngressNginxServerAliasAnnotation] = aliases
	return ing
}

// A server alias never takes a host away: one that is a host of another resource, or that a wildcard alias
// matches, is dropped with a warning, as is one an older Ingress has as its alias; a host of its own is no conflict.
func TestServerAliasesRankBelowHosts(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	now := time.Now()

	vs := createTestVirtualServer("vs", "shop.example.com")
	vs.CreationTimestamp.Time = now
	c.AddOrUpdateVirtualServer(vs)
	victim := createTestIngressWithPath("victim", "login.example.com", "/", "svc", now)
	victim.Namespace = "victim"
	c.AddOrUpdateIngress(victim)
	older := withServerAlias(createTestIngressWithPath("older", "older.example.com", "/", "svc", now.Add(-2*time.Hour)), "www.older.example.com")
	c.AddOrUpdateIngress(older)

	// older than all of them, so a host would win - an alias does not
	alias := withServerAlias(createTestIngressWithPath("alias", "alias.example.com", "/", "svc", now.Add(-time.Hour)),
		"SHOP.example.com, *.example.com,www.older.example.com,alias.example.com,mine.example.com")
	alias.Namespace = "tenant"
	c.AddOrUpdateIngress(alias)

	holder := holderOf(t, c, "alias.example.com")
	for _, dropped := range []string{"shop.example.com", "*.example.com", "www.older.example.com"} {
		if !holder.DroppedServerAliases[dropped] {
			t.Errorf("%s is not dropped: %v", dropped, holder.DroppedServerAliases)
		}
	}
	for _, kept := range []string{"alias.example.com", "mine.example.com"} {
		if holder.DroppedServerAliases[kept] {
			t.Errorf("%s is dropped", kept)
		}
	}
	if len(holder.Warnings) != 3 {
		t.Errorf("warnings %v, want one per dropped alias", holder.Warnings)
	}
	for _, w := range holder.Warnings {
		if strings.Contains(w, "victim") || strings.Contains(w, "vs") && !strings.Contains(w, "server-alias") {
			t.Errorf("the warning %q names another resource", w)
		}
	}
	if holderOf(t, c, "older.example.com").DroppedServerAliases != nil {
		t.Errorf("the older Ingress lost its alias")
	}

	// the config is made of the Ingress without the dropped aliases
	ing := holder.ingressForServer()
	if ing == holder.Ingress {
		t.Fatal("the config is made of the Ingress with all its aliases")
	}
	if got := ing.Annotations[configs.IngressNginxServerAliasAnnotation]; strings.Contains(got, "shop") || strings.Contains(got, "*") || !strings.Contains(got, "mine.example.com") {
		t.Errorf("aliases of the config: %q", got)
	}

	// once the VirtualServer is gone, its host is the alias's: the config changes
	changes, _ := c.DeleteVirtualServer("default/vs")
	var changed bool
	for _, ch := range changes {
		if ch.Resource.GetKeyWithKind() == "Ingress/tenant/alias" {
			changed = true
		}
	}
	if !changed || holderOf(t, c, "alias.example.com").DroppedServerAliases["shop.example.com"] {
		t.Errorf("after the VirtualServer went: changes %v, dropped %v", changes, holderOf(t, c, "alias.example.com").DroppedServerAliases)
	}
}

func TestServerNamesCollide(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"a.example.com", "a.example.com", true},
		{"*.example.com", "a.example.com", true},
		{"a.example.com", "*.example.com", true},
		{"*.example.com", "a.b.example.com", true},
		{"*.example.com", "example.com", false},
		{"*.example.com", "aexample.com", false},
		{"a.example.com", "b.example.com", false},
		// nginx prefers the longer wildcard: *.a.example.com takes x.a.example.com from *.example.com
		{"*.a.example.com", "*.example.com", true},
	} {
		if got := serverNamesCollide(tc.a, tc.b); got != tc.want {
			t.Errorf("serverNamesCollide(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// The webhook refuses a path another namespace's Ingress has without naming that Ingress or its namespace.
func TestIngressOverlapOfAnotherNamespaceNamesNoIngress(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	c.AddOrUpdateIngress(createTestIngressWithPath("secret-app", "cafe.example.com", "/tea", "tea", time.Now()))

	ing := createTestIngressWithPath("probe", "cafe.example.com", "/tea", "tea", time.Now())
	ing.Namespace = "tenant"
	err := c.ingressOverlap("tenant/probe", ing)
	if err == nil {
		t.Fatal("the overlap is not refused")
	}
	if strings.Contains(err.Error(), "secret-app") || strings.Contains(err.Error(), "default") {
		t.Errorf("the refusal names the other Ingress: %v", err)
	}
	if !strings.Contains(err.Error(), "already defined in another Ingress") {
		t.Errorf("refusal: %v", err)
	}
}

type blockingChecker struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingChecker) CheckIngress(*networking.Ingress) ([]string, error) {
	b.entered <- struct{}{}
	<-b.release
	return nil, nil
}

func ingressReview() []byte {
	return []byte(`{"apiVersion":"admission.k8s.io/v1","kind":"AdmissionReview","request":{"uid":"1",` +
		`"kind":{"group":"networking.k8s.io","version":"v1","kind":"Ingress"},"operation":"CREATE","namespace":"default","name":"a",` +
		`"object":{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"a","namespace":"default"}}}}`)
}

// The webhook takes maxAdmissionChecks reviews at once and answers 503 to more, and refuses reviews too big.
func TestAdmissionHandlerLimits(t *testing.T) {
	t.Parallel()
	checker := &blockingChecker{entered: make(chan struct{}, maxAdmissionChecks), release: make(chan struct{})}
	server := httptest.NewServer(AdmissionHandler(checker))
	defer server.Close()

	var wg sync.WaitGroup
	for range maxAdmissionChecks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(server.URL, "application/json", bytes.NewReader(ingressReview()))
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("a review within the limit: %d", resp.StatusCode)
				}
			}
		}()
	}
	for range maxAdmissionChecks {
		<-checker.entered
	}
	resp, err := http.Post(server.URL, "application/json", bytes.NewReader(ingressReview()))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("one more review: %d, want 503", resp.StatusCode)
	}
	close(checker.release)
	wg.Wait()

	big := bytes.Repeat([]byte(" "), maxAdmissionReview+1)
	resp, err = http.Post(server.URL, "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a review too big: %d, want 413", resp.StatusCode)
	}

	// without a length the body is cut at the limit
	req, _ := http.NewRequest(http.MethodPost, server.URL, struct{ *bytes.Reader }{bytes.NewReader(big)})
	req.ContentLength = -1
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a review too big without a length: %d, want 413", resp.StatusCode)
	}
}

// A sync that panics is logged and its task synced again, instead of the process crashing.
func TestSyncPanicIsRecoveredAndRequeued(t *testing.T) {
	old := panicRequeueDelay
	panicRequeueDelay = 10 * time.Millisecond
	defer func() { panicRequeueDelay = old }()

	var mu sync.Mutex
	calls := 0
	done := make(chan struct{})
	tq := newTaskQueue(nl.LoggerFromContext(t.Context()), func(task) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		switch calls {
		case 1:
			panic(errors.New("crafted object"))
		case 2:
			close(done)
		}
	})
	go tq.Run(time.Millisecond, t.Context().Done())
	defer tq.Shutdown()
	tq.Add(task{Kind: ingress, Key: "default/crafted"})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the task was not synced again after the panic")
	}
}
