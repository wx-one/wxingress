package configs

import "fmt"

// RenderedConfig is the config file an Ingress config would have: its name in conf.d (without .conf) and its content.
type RenderedConfig struct {
	Name    string
	Content []byte
}

// RenderIngress returns the config file the Ingress would have and its
// warnings, without writing it or changing what the Configurator keeps
// (App Protect's files are not written: their paths are rendered as the
// policies name them).
func (cnf *Configurator) RenderIngress(ingEx *IngressEx) (RenderedConfig, Warnings, error) {
	nginxCfg, warnings := generateNginxCfg(NginxCfgParams{
		staticParams:              cnf.staticCfgParams,
		ingEx:                     ingEx,
		apResources:               &AppProtectResources{},
		BaseCfgParams:             cnf.CfgParams,
		isPlus:                    cnf.isPlus,
		isResolverConfigured:      cnf.IsResolverConfigured(),
		isWildcardEnabled:         cnf.isWildcardEnabled,
		ingressControllerReplicas: cnf.ingressControllerReplicas,
		defaultBackend:            cnf.ingressNginxDefaultBackend(),
	})
	name := objectMetaToFileName(&ingEx.Ingress.ObjectMeta)
	if ingEx.ValidHosts[emptyHostName] {
		name = DefaultServerConfigName
	}
	content, err := cnf.templateExecutor.ExecuteIngressConfigTemplate(&nginxCfg)
	if err != nil {
		return RenderedConfig{}, warnings, fmt.Errorf("error generating Ingress Config %v: %w", name, err)
	}
	return RenderedConfig{Name: name, Content: content}, warnings, nil
}

// RenderMergeableIngresses is RenderIngress of a master and its minions.
func (cnf *Configurator) RenderMergeableIngresses(mergeableIngs *MergeableIngresses) (RenderedConfig, Warnings, error) {
	nginxCfg, warnings := generateNginxCfgForMergeableIngresses(NginxCfgParams{
		mergeableIngs:             mergeableIngs,
		apResources:               &AppProtectResources{},
		BaseCfgParams:             cnf.CfgParams,
		isPlus:                    cnf.isPlus,
		isResolverConfigured:      cnf.IsResolverConfigured(),
		staticParams:              cnf.staticCfgParams,
		isWildcardEnabled:         cnf.isWildcardEnabled,
		ingressControllerReplicas: cnf.ingressControllerReplicas,
		defaultBackend:            cnf.ingressNginxDefaultBackend(),
	})
	name := objectMetaToFileName(&mergeableIngs.Master.Ingress.ObjectMeta)
	if mergeableIngs.Master.ValidHosts[emptyHostName] {
		name = DefaultServerConfigName
	}
	content, err := cnf.templateExecutor.ExecuteIngressConfigTemplate(&nginxCfg)
	if err != nil {
		return RenderedConfig{}, warnings, fmt.Errorf("error generating Ingress Config %v: %w", name, err)
	}
	return RenderedConfig{Name: name, Content: content}, warnings, nil
}

// TestConfigs runs nginx -t of the configuration with the config files
// replaced - or added - by those given, leaving the configuration as it is.
func (cnf *Configurator) TestConfigs(configs []RenderedConfig) error {
	tester, ok := cnf.nginxManager.(interface {
		TestWithConfigs(map[string][]byte) error
	})
	if !ok {
		return nil
	}
	replaced := make(map[string][]byte, len(configs))
	for _, c := range configs {
		replaced[c.Name] = c.Content
	}
	return tester.TestWithConfigs(replaced)
}
