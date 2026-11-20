package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	nl "github.com/wx-one/wxingress/internal/logger"
	"github.com/wx-one/wxingress/internal/nginx"
	api_v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// The revocations of oidc sessions live in a ConfigMap, one per line under
// the key "revocations":
//
//	all <unix>            every session made before
//	sub <sub> <unix>      the sessions of a user made before
//	sid <sid> <unix>      the sessions of an IdP session
//	ses <id> <unix>       one session (what /logout ends)
//
// An administrator ends sessions by adding a line. Every replica's controller
// gives its wxingress module the whole list every few seconds - no reload,
// and a restarted nginx has it again at once - and writes into the ConfigMap
// what a logout added in its module (the IdP's back- or front-channel logout,
// /logout), so that every replica learns it. Lines older than a month go:
// sessions are refreshed long before, and a refreshed one is new.

// OIDCRevocationsKey is the key of the ConfigMap the lines are under.
const OIDCRevocationsKey = "revocations"

const oidcRevocationsKept = 30 * 24 * time.Hour

type oidcRevocationsModule interface {
	SetOIDCRevocations(r nginx.OIDCRevocations) error
	LocalOIDCRevocations() ([]nginx.OIDCRevocation, error)
}

// ParseOIDCRevocations reads the lines of the ConfigMap.
func ParseOIDCRevocations(text string) (nginx.OIDCRevocations, []string) {
	var r nginx.OIDCRevocations
	var wrong []string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch {
		case f[0] == "all" && len(f) == 2:
			if t, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				r.All = max(r.All, t)
				continue
			}
		case (f[0] == "sub" || f[0] == "sid" || f[0] == "ses") && len(f) == 3:
			if t, err := strconv.ParseInt(f[2], 10, 64); err == nil {
				r.Entries = append(r.Entries, nginx.OIDCRevocation{Key: f[0] + ":" + f[1], Before: t})
				continue
			}
		}
		wrong = append(wrong, line)
	}
	return r, wrong
}

// FormatOIDCRevocations writes the lines, sorted, without the ones older than kept.
func FormatOIDCRevocations(r nginx.OIDCRevocations, now time.Time) string {
	byKey := map[string]int64{}
	for _, e := range r.Entries {
		if time.Unix(e.Before, 0).Add(oidcRevocationsKept).Before(now) {
			continue
		}
		byKey[e.Key] = max(byKey[e.Key], e.Before)
	}
	var lines []string
	if r.All != 0 {
		lines = append(lines, fmt.Sprintf("all %d", r.All))
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		kind, value, _ := strings.Cut(k, ":")
		lines = append(lines, fmt.Sprintf("%s %s %d", kind, value, byKey[k]))
	}
	return strings.Join(lines, "\n") + "\n"
}

// RunOIDCRevocations keeps the module of this replica and the ConfigMap in step until ctx ends.
func RunOIDCRevocations(ctx context.Context, logger *slog.Logger, client kubernetes.Interface, namespace, name string, manager nginx.Manager, every time.Duration) {
	module, ok := manager.(oidcRevocationsModule)
	if !ok {
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := syncOIDCRevocations(ctx, client, namespace, name, module, time.Now()); err != nil {
			nl.Warnf(logger, "oidc revocations: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func syncOIDCRevocations(ctx context.Context, client kubernetes.Interface, namespace, name string, module oidcRevocationsModule, now time.Time) error {
	local, err := module.LocalOIDCRevocations()
	if err != nil {
		return err
	}
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, meta_v1.GetOptions{})
	if apierrors.IsNotFound(err) {
		cm = &api_v1.ConfigMap{ObjectMeta: meta_v1.ObjectMeta{Name: name, Namespace: namespace}}
		err = nil
	}
	if err != nil {
		return fmt.Errorf("error reading the ConfigMap %s/%s: %w", namespace, name, err)
	}
	listed, _ := ParseOIDCRevocations(cm.Data[OIDCRevocationsKey])

	// what a logout added here and the ConfigMap has not: written down for every replica
	have := map[string]int64{}
	for _, e := range listed.Entries {
		have[e.Key] = e.Before
	}
	added := false
	for _, e := range local {
		if before, ok := have[e.Key]; !ok || before < e.Before {
			listed.Entries = append(listed.Entries, nginx.OIDCRevocation{Key: e.Key, Before: e.Before})
			added = true
		}
	}
	if added {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[OIDCRevocationsKey] = FormatOIDCRevocations(listed, now)
		if cm.ResourceVersion == "" {
			_, err = client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, meta_v1.CreateOptions{})
		} else {
			_, err = client.CoreV1().ConfigMaps(namespace).Update(ctx, cm, meta_v1.UpdateOptions{})
		}
		if err != nil {
			// another replica wrote first: the next round merges again
			return fmt.Errorf("error writing the ConfigMap %s/%s: %w", namespace, name, err)
		}
	}
	return module.SetOIDCRevocations(listed)
}
