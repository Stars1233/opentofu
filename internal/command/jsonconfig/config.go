// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package jsonconfig

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/getproviders"
	"github.com/opentofu/opentofu/internal/tofu"
)

// Config represents the complete configuration source
type config struct {
	ProviderConfigs map[string]providerConfig `json:"provider_config,omitempty"`
	RootModule      module                    `json:"root_module,omitempty"`
}

// ProviderConfig describes all of the provider configurations throughout the
// configuration tree, flattened into a single map for convenience since
// provider configurations are the one concept in OpenTofu that can span across
// module boundaries.
type providerConfig struct {
	Name              string         `json:"name,omitempty"`
	FullName          string         `json:"full_name,omitempty"`
	Alias             string         `json:"alias,omitempty"`
	VersionConstraint string         `json:"version_constraint,omitempty"`
	ModuleAddress     string         `json:"module_address,omitempty"`
	Expressions       map[string]any `json:"expressions,omitempty"`
	parentKey         string
}

type module struct {
	Outputs map[string]output `json:"outputs,omitempty"`
	// Resources are sorted in a user-friendly order that is undefined at this
	// time, but consistent.
	Resources   []resource            `json:"resources,omitempty"`
	ModuleCalls map[string]moduleCall `json:"module_calls,omitempty"`
	Variables   variables             `json:"variables,omitempty"`
}

type moduleCall struct {
	Source            string         `json:"source,omitempty"`
	Expressions       map[string]any `json:"expressions,omitempty"`
	CountExpression   *expression    `json:"count_expression,omitempty"`
	ForEachExpression *expression    `json:"for_each_expression,omitempty"`
	Module            *module        `json:"module,omitempty"`
	VersionConstraint string         `json:"version_constraint,omitempty"`
	DependsOn         []string       `json:"depends_on,omitempty"`
}

// variables is the JSON representation of the variables provided to the current
// plan.
type variables map[string]*variable

type variable struct {
	Type        json.RawMessage `json:"type,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
	Deprecated  string          `json:"deprecated,omitempty"`
}

// Resource is the representation of a resource in the config
type resource struct {
	// Address is the absolute resource address
	Address string `json:"address,omitempty"`

	// Mode can be "managed" or "data"
	Mode string `json:"mode,omitempty"`

	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`

	// ProviderConfigKey is the key into "provider_configs" (shown above) for
	// the provider configuration that this resource is associated with.
	//
	// NOTE: If a given resource is in a ModuleCall, and the provider was
	// configured outside of the module (in a higher level configuration file),
	// the ProviderConfigKey will not match a key in the ProviderConfigs map.
	ProviderConfigKey string `json:"provider_config_key,omitempty"`

	// Provisioners is an optional field which describes any provisioners.
	// Connection info will not be included here.
	Provisioners []provisioner `json:"provisioners,omitempty"`

	// Expressions" describes the resource-type-specific  content of the
	// configuration block.
	Expressions map[string]any `json:"expressions,omitempty"`

	// SchemaVersion indicates which version of the resource type schema the
	// "values" property conforms to.
	SchemaVersion *uint64 `json:"schema_version,omitempty"`

	// CountExpression and ForEachExpression describe the expressions given for
	// the corresponding meta-arguments in the resource configuration block.
	// These are omitted if the corresponding argument isn't set.
	CountExpression   *expression `json:"count_expression,omitempty"`
	ForEachExpression *expression `json:"for_each_expression,omitempty"`

	DependsOn []string `json:"depends_on,omitempty"`
}

type output struct {
	Sensitive   bool        `json:"sensitive,omitempty"`
	Deprecated  string      `json:"deprecated,omitempty"`
	Expression  *expression `json:"expression,omitempty"`
	DependsOn   []string    `json:"depends_on,omitempty"`
	Description string      `json:"description,omitempty"`
}

type provisioner struct {
	Type        string         `json:"type,omitempty"`
	Expressions map[string]any `json:"expressions,omitempty"`
}

// Marshal returns the json encoding of tofu configuration.
func Marshal(c *configs.Config, schemas *tofu.Schemas) ([]byte, error) {
	return marshal(c, schemas)
}

// marshal is the shared implementation of both [Marshal] and
// [MarshalSingleModule].
//
// [MarshalSingleModule] calls this with a synthetic [configs.Config] that
// has only a root module, and with schemas set to nil. Downstream codepaths
// should test for single module mode only by passing the schemas value to
// [inSingleModuleMode], and not by directly testing if schemas are nil,
// so that it's easier for future maintainers to learn about this special
// treatment through the centralized doc comment.
func marshal(c *configs.Config, schemas *tofu.Schemas) ([]byte, error) {
	var output config

	pcs := make(map[string]providerConfig)
	marshalProviderConfigs(c, schemas, pcs)

	rootModule, err := marshalModule(c, schemas, "")
	if err != nil {
		return nil, err
	}
	output.RootModule = rootModule

	normalizeModuleProviderKeys(&rootModule, pcs)

	for name, pc := range pcs {
		if pc.parentKey != "" {
			delete(pcs, name)
		}
	}
	output.ProviderConfigs = pcs

	ret, err := json.Marshal(output)
	return ret, err
}

func marshalProviderConfigs(
	c *configs.Config,
	schemas *tofu.Schemas,
	m map[string]providerConfig,
) {
	if c == nil {
		return
	}

	// We want to determine only the provider requirements from this module,
	// ignoring any descendants.  Disregard any diagnostics when determining
	// requirements because we want this marshalling to succeed even if there
	// are invalid constraints.
	reqs, _ := c.ProviderRequirementsShallow()

	// Add an entry for each provider configuration block in the module.
	for k, pc := range c.Module.ProviderConfigs {
		providerFqn := c.ProviderForConfigAddr(addrs.LocalProviderConfig{LocalName: pc.Name})
		schema := mapSchema(schemas, func(schemas *tofu.Schemas) *configschema.Block {
			return schemas.ProviderConfig(providerFqn)
		})

		p := providerConfig{
			Name:          pc.Name,
			FullName:      providerFqn.String(),
			Alias:         pc.Alias,
			ModuleAddress: c.Path.String(),
			Expressions:   marshalExpressions(pc.Config, schema),
		}

		// Store the fully resolved provider version constraint, rather than
		// using the version argument in the configuration block. This is both
		// future proof (for when we finish the deprecation of the provider config
		// version argument) and more accurate (as it reflects the full set of
		// constraints, in case there are multiple).
		if vc, ok := reqs[providerFqn]; ok {
			p.VersionConstraint = getproviders.VersionConstraintsString(vc)
		}

		key := opaqueProviderKey(k, c.Path.String())

		m[key] = p
	}

	// Ensure that any required providers with no associated configuration
	// block are included in the set.
	for k, pr := range c.Module.ProviderRequirements.RequiredProviders {
		// If a provider has aliases defined, process those first.
		for _, alias := range pr.Aliases {
			// If there exists a value for this provider, we have nothing to add
			// to it, so skip.
			key := opaqueProviderKey(alias.StringCompact(), c.Path.String())
			if _, exists := m[key]; exists {
				continue
			}
			// Given no provider configuration block exists, the only fields we can
			// fill here are the local name, FQN, module address, and version
			// constraints.
			p := providerConfig{
				Name:          pr.Name,
				FullName:      pr.Type.String(),
				ModuleAddress: c.Path.String(),
			}

			if vc, ok := reqs[pr.Type]; ok {
				p.VersionConstraint = getproviders.VersionConstraintsString(vc)
			}

			m[key] = p
		}

		// If there exists a value for this provider, we have nothing to add
		// to it, so skip.
		key := opaqueProviderKey(k, c.Path.String())
		if _, exists := m[key]; exists {
			continue
		}

		// Given no provider configuration block exists, the only fields we can
		// fill here are the local name, module address, and version
		// constraints.
		p := providerConfig{
			Name:          pr.Name,
			FullName:      pr.Type.String(),
			ModuleAddress: c.Path.String(),
		}

		if vc, ok := reqs[pr.Type]; ok {
			p.VersionConstraint = getproviders.VersionConstraintsString(vc)
		}

		if c.Parent != nil {
			parentKey := opaqueProviderKey(pr.Name, c.Parent.Path.String())
			p.parentKey = findSourceProviderKey(parentKey, p.FullName, m)
		}

		m[key] = p
	}

	// Providers could be implicitly created or inherited from the parent module
	// when no requirements and configuration block defined.
	for req := range reqs {
		// Only default providers could implicitly exist,
		// so the provider name must be same as the provider type.
		key := opaqueProviderKey(req.Type, c.Path.String())
		if _, exists := m[key]; exists {
			continue
		}

		p := providerConfig{
			Name:          req.Type,
			FullName:      req.String(),
			ModuleAddress: c.Path.String(),
		}

		// In child modules, providers defined in the parent module can be implicitly used.
		if c.Parent != nil {
			parentKey := opaqueProviderKey(req.Type, c.Parent.Path.String())
			p.parentKey = findSourceProviderKey(parentKey, p.FullName, m)
		}

		m[key] = p
	}

	// Must also visit our child modules, recursively.
	for name, mc := range c.Module.ModuleCalls {
		// Keys in c.Children are guaranteed to match those in c.Module.ModuleCalls
		cc := c.Children[name]

		// Add provider config map entries for passed provider configs,
		// pointing at the passed configuration
		for _, ppc := range mc.Providers {
			// These provider names include aliases, if set
			moduleProviderName := ppc.InChild.String()
			parentProviderName := ppc.InParent.String()

			// Look up the provider FQN from the module context, using the non-aliased local name
			providerFqn := cc.ProviderForConfigAddr(addrs.LocalProviderConfig{LocalName: ppc.InChild.Name})

			// The presence of passed provider configs means that we cannot have
			// any configuration expressions or version constraints here
			p := providerConfig{
				Name:          moduleProviderName,
				FullName:      providerFqn.String(),
				ModuleAddress: cc.Path.String(),
			}

			key := opaqueProviderKey(moduleProviderName, cc.Path.String())
			parentKey := opaqueProviderKey(parentProviderName, cc.Parent.Path.String())
			p.parentKey = findSourceProviderKey(parentKey, p.FullName, m)

			m[key] = p
		}

		// Finally, marshal any other provider configs within the called module.
		// It is safe to do this last because it is invalid to configure a
		// provider which has passed provider configs in the module call.
		// We don't recurse in single-module mode, because cc will be nil in
		// that case.
		if !inSingleModuleMode(schemas) {
			marshalProviderConfigs(cc, schemas, m)
		}
	}
}

func marshalModule(c *configs.Config, schemas *tofu.Schemas, addr string) (module, error) {
	var module module
	var rs []resource

	managedResources, err := marshalResources(c.Module.ManagedResources, schemas, addr)
	if err != nil {
		return module, err
	}
	dataResources, err := marshalResources(c.Module.DataResources, schemas, addr)
	if err != nil {
		return module, err
	}

	rs = append(managedResources, dataResources...)
	module.Resources = rs

	outputs := make(map[string]output)
	for _, v := range c.Module.Outputs {
		o := output{
			Sensitive:  v.Sensitive,
			Deprecated: v.Deprecated,
		}
		if !inSingleModuleMode(schemas) {
			expr := marshalExpression(v.Expr)
			o.Expression = &expr
		}
		if v.Description != "" {
			o.Description = v.Description
		}
		if len(v.DependsOn) > 0 {
			dependencies := make([]string, len(v.DependsOn))
			for i, d := range v.DependsOn {
				ref, diags := addrs.ParseRef(d)
				// we should not get an error here, because `tofu validate`
				// would have complained well before this point, but if we do we'll
				// silently skip it.
				if !diags.HasErrors() {
					dependencies[i] = ref.Subject.String()
				}
			}
			o.DependsOn = dependencies
		}

		outputs[v.Name] = o
	}
	module.Outputs = outputs

	module.ModuleCalls = marshalModuleCalls(c, schemas)

	if len(c.Module.Variables) > 0 {
		vars := make(variables, len(c.Module.Variables))
		for k, v := range c.Module.Variables {
			typeConstraint := cty.DynamicPseudoType
			if v.ConstraintType != cty.NilType {
				typeConstraint = v.ConstraintType
			}

			var typeJSON []byte
			// We leave the "type" property unset in output when it
			// would be DynamicPseudoType, because the most typical way to
			// represent this situation in our source language is to
			// omit the type argument from the declaration -- it essentially
			// represents "no type constrant at all" -- and because this
			// avoids exposing a potentially-confusing detail that cty
			// describes DynamicPseudoType as "dynamic" in JSON, while HCL
			// prefers to call it "any".
			if !typeConstraint.Equals(cty.DynamicPseudoType) {
				typeJSON, err = typeConstraint.MarshalJSON()
				if err != nil {
					// Should not get here, because v.ConstraintType should always
					// be a valid cty type when it isn't NilType, so this uses
					// the internal type stringification to get the most detailed
					// error message in a potential bug report.
					return module, fmt.Errorf("failed to marshal %#v as JSON: %w", typeConstraint, err)
				}
			}

			var defaultValJSON []byte
			var required bool
			if v.Default == cty.NilVal {
				defaultValJSON = nil
				required = true
			} else {
				defaultValJSON, err = ctyjson.Marshal(v.Default, v.Default.Type())
				required = false
				if err != nil {
					return module, err
				}
			}
			vars[k] = &variable{
				Type:        typeJSON,
				Default:     defaultValJSON,
				Required:    required,
				Description: v.Description,
				Sensitive:   v.Sensitive,
				Deprecated:  v.Deprecated,
			}
		}
		module.Variables = vars
	}

	return module, nil
}

func marshalModuleCalls(c *configs.Config, schemas *tofu.Schemas) map[string]moduleCall {
	ret := make(map[string]moduleCall)

	for name, mc := range c.Module.ModuleCalls {
		mcConfig := c.Children[name]
		ret[name] = marshalModuleCall(mcConfig, mc, schemas)
	}

	return ret
}

func marshalModuleCall(c *configs.Config, mc *configs.ModuleCall, schemas *tofu.Schemas) moduleCall {
	// Note that "c" is always nil when in single module mode!
	// Refer to the docs on [inSingleModuleMode] to learn about how that
	// special situation works.

	ret := moduleCall{
		// We're intentionally echoing back exactly what the user entered
		// here, rather than the normalized version in SourceAddr, because
		// historically we only _had_ the raw address and thus it would be
		// a (admittedly minor) breaking change to start normalizing them
		// now, in case consumers of this data are expecting a particular
		// non-normalized syntax.
		Source:            mc.SourceAddrRaw,
		VersionConstraint: mc.Version.Required.String(),
	}

	if !inSingleModuleMode(schemas) {
		// The expression-related properties are not available in single-module
		// mode.
		cExp := marshalExpression(mc.Count)
		if !cExp.Empty() {
			ret.CountExpression = &cExp
		} else {
			fExp := marshalExpression(mc.ForEach)
			if !fExp.Empty() {
				ret.ForEachExpression = &fExp
			}
		}
		schema := &configschema.Block{}
		schema.Attributes = make(map[string]*configschema.Attribute)
		for _, variable := range c.Module.Variables {
			schema.Attributes[variable.Name] = &configschema.Attribute{
				Required: variable.Default == cty.NilVal,
			}
		}
		ret.Expressions = marshalExpressions(mc.Config, schema)

		// The "module" property, describing the content of the child module,
		// is not available in single-module mode.
		module, _ := marshalModule(c, schemas, c.Path.String())
		ret.Module = &module
	}

	if len(mc.DependsOn) > 0 {
		dependencies := make([]string, len(mc.DependsOn))
		for i, d := range mc.DependsOn {
			ref, diags := addrs.ParseRef(d)
			// we should not get an error here, because `tofu validate`
			// would have complained well before this point, but if we do we'll
			// silently skip it.
			if !diags.HasErrors() {
				dependencies[i] = ref.Subject.String()
			}
		}
		ret.DependsOn = dependencies
	}

	return ret
}

func marshalResources(resources map[string]*configs.Resource, schemas *tofu.Schemas, moduleAddr string) ([]resource, error) {
	var rs []resource
	for _, v := range resources {
		providerConfigKey := opaqueProviderKey(v.ProviderConfigAddr().StringCompact(), moduleAddr)
		r := resource{
			Address:           v.Addr().String(),
			Type:              v.Type,
			Name:              v.Name,
			ProviderConfigKey: providerConfigKey,
		}

		switch v.Mode {
		case addrs.ManagedResourceMode:
			r.Mode = "managed"
		case addrs.DataResourceMode:
			r.Mode = "data"
		default:
			return rs, fmt.Errorf("resource %s has an unsupported mode %s", r.Address, v.Mode.String())
		}

		if !inSingleModuleMode(schemas) {
			// We don't populate the expression and schema-related properties
			// when we are in single-module mode.
			cExp := marshalExpression(v.Count)
			if !cExp.Empty() {
				r.CountExpression = &cExp
			} else {
				fExp := marshalExpression(v.ForEach)
				if !fExp.Empty() {
					r.ForEachExpression = &fExp
				}
			}

			schema, schemaVer := schemas.ResourceTypeConfig(
				v.Provider,
				v.Mode,
				v.Type,
			)
			if schema == nil {
				return nil, fmt.Errorf("no schema found for %s (in provider %s)", v.Addr().String(), v.Provider)
			}
			r.SchemaVersion = &schemaVer
			r.Expressions = marshalExpressions(v.Config, schema)
		}

		// Managed is populated only for Mode = addrs.ManagedResourceMode
		if v.Managed != nil && len(v.Managed.Provisioners) > 0 {
			var provisioners []provisioner
			for _, p := range v.Managed.Provisioners {
				schema := mapSchema(schemas, func(schema *tofu.Schemas) *configschema.Block {
					return schemas.ProvisionerConfig(p.Type)
				})
				prov := provisioner{
					Type:        p.Type,
					Expressions: marshalExpressions(p.Config, schema),
				}
				provisioners = append(provisioners, prov)
			}
			r.Provisioners = provisioners
		}

		if len(v.DependsOn) > 0 {
			dependencies := make([]string, len(v.DependsOn))
			for i, d := range v.DependsOn {
				ref, diags := addrs.ParseRef(d)
				// we should not get an error here, because `tofu validate`
				// would have complained well before this point, but if we do we'll
				// silently skip it.
				if !diags.HasErrors() {
					dependencies[i] = ref.Subject.String()
				}
			}
			r.DependsOn = dependencies
		}

		rs = append(rs, r)
	}
	sort.Slice(rs, func(i, j int) bool {
		return rs[i].Address < rs[j].Address
	})
	return rs, nil
}

// Flatten all resource provider keys in a module and its descendents, such
// that any resources from providers using a configuration passed through the
// module call have a direct reference to that provider configuration.
func normalizeModuleProviderKeys(m *module, pcs map[string]providerConfig) {
	for i, r := range m.Resources {
		if pc, exists := pcs[r.ProviderConfigKey]; exists {
			if _, hasParent := pcs[pc.parentKey]; hasParent {
				m.Resources[i].ProviderConfigKey = pc.parentKey
			}
		}
	}

	for _, mc := range m.ModuleCalls {
		if mc.Module == nil {
			// This field is not populated in single-module mode, but
			// that's okay because it means we have no need to recurse
			// into it for nested fixups.
			continue
		}
		normalizeModuleProviderKeys(mc.Module, pcs)
	}
}

// opaqueProviderKey generates a unique absProviderConfig-like string from the module
// address and provider
func opaqueProviderKey(provider string, addr string) (key string) {
	key = provider
	if addr != "" {
		key = fmt.Sprintf("%s:%s", addr, provider)
	}
	return key
}

// Traverse up the module call tree until we find the provider
// configuration which has no linked parent config. This is then
// the source of the configuration used in this module call, so
// we link to it directly
func findSourceProviderKey(startKey string, fullName string, m map[string]providerConfig) string {
	var parentKey string

	key := startKey
	for key != "" {
		parent, exists := m[key]
		if !exists || parent.FullName != fullName {
			break
		}

		parentKey = key
		key = parent.parentKey
	}

	return parentKey
}
