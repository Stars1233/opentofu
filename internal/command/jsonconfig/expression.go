// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package jsonconfig

import (
	"encoding/json"
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/lang"
	"github.com/opentofu/opentofu/internal/lang/blocktoattr"
)

// expression represents any unparsed expression
type expression struct {
	// "constant_value" is set only if the expression contains no references to
	// other objects, in which case it gives the resulting constant value. This
	// is mapped as for the individual values in the common value
	// representation.
	ConstantValue json.RawMessage `json:"constant_value,omitempty"`

	// Alternatively, "references" will be set to a list of references in the
	// expression. Multi-step references will be unwrapped and duplicated for
	// each significant traversal step, allowing callers to more easily
	// recognize the objects they care about without attempting to parse the
	// expressions. Callers should only use string equality checks here, since
	// the syntax may be extended in future releases.
	References []string `json:"references,omitempty"`
}

func marshalExpression(ex hcl.Expression) expression {
	var ret expression
	if ex == nil {
		return ret
	}

	val, valueDiags := ex.Value(nil)
	if val != cty.NilVal && !valueDiags.HasErrors() {
		valJSON, _ := ctyjson.Marshal(val, val.Type())
		ret.ConstantValue = valJSON
	}

	refs, _ := lang.ReferencesInExpr(addrs.ParseRef, ex)
	if len(refs) > 0 {
		var varString []string
		for _, ref := range refs {
			// We work backwards here, starting with the full reference +
			// remaining traversal, and then unwrapping the remaining traversals
			// into parts until we end up at the smallest referenceable address.
			remains := ref.Remaining
			for len(remains) > 0 {
				varString = append(varString, fmt.Sprintf("%s%s", ref.Subject, addrs.TraversalStr(remains)))
				remains = remains[:(len(remains) - 1)]
			}
			varString = append(varString, ref.Subject.String())

			switch ref.Subject.(type) {
			case addrs.ModuleCallInstance:
				if ref.Subject.(addrs.ModuleCallInstance).Key != addrs.NoKey {
					// Include the module call, without the key
					varString = append(varString, ref.Subject.(addrs.ModuleCallInstance).Call.String())
				}
			case addrs.ResourceInstance:
				if ref.Subject.(addrs.ResourceInstance).Key != addrs.NoKey {
					// Include the resource, without the key
					varString = append(varString, ref.Subject.(addrs.ResourceInstance).Resource.String())
				}
			case addrs.ModuleCallInstanceOutput:
				// Include the module name, without the output name
				varString = append(varString, ref.Subject.(addrs.ModuleCallInstanceOutput).Call.String())
			}
		}
		ret.References = varString
	}

	return ret
}

func (e *expression) Empty() bool {
	return e.ConstantValue == nil && e.References == nil
}

// expressions is used to represent the entire content of a block. Attribute
// arguments are mapped directly with the attribute name as key and an
// expression as value.
type expressions map[string]any

// marshalExpressions returns a representation of the expressions in the given
// body after analyzing based on the given schema.
//
// If [inSingleModuleMode] returns true when given schema, the result is always
// nil to represent that expression information is not available in
// single-module mode.
func marshalExpressions(body hcl.Body, schema *configschema.Block) expressions {
	if inSingleModuleMode(schema) {
		// We never generate any expressions in single-module mode.
		return nil
	}

	// Since we want the raw, un-evaluated expressions we need to use the
	// low-level HCL API here, rather than the hcldec decoder API. That means we
	// need the low-level schema.
	lowSchema := hcldec.ImpliedSchema(schema.DecoderSpec())
	// (lowSchema is an hcl.BodySchema:
	// https://godoc.org/github.com/hashicorp/hcl/v2/hcl#BodySchema )

	// fix any ConfigModeAttr blocks present from legacy providers
	body = blocktoattr.FixUpBlockAttrs(body, schema)

	// Use the low-level schema with the body to decode one level We'll just
	// ignore any additional content that's not covered by the schema, which
	// will effectively ignore "dynamic" blocks, and may also ignore other
	// unknown stuff but anything else would get flagged by OpenTofu as an
	// error anyway, and so we wouldn't end up in here.
	content, _, _ := body.PartialContent(lowSchema)
	if content == nil {
		// Should never happen for a valid body, but we'll just generate empty
		// if there were any problems.
		return nil
	}

	ret := make(expressions)

	// Any attributes we encode directly as expression objects.
	for name, attr := range content.Attributes {
		ret[name] = marshalExpression(attr.Expr) // note: singular expression for this one
	}

	// Any nested blocks require a recursive call to produce nested expressions
	// objects.
	for _, block := range content.Blocks {
		typeName := block.Type
		blockS, exists := schema.BlockTypes[typeName]
		if !exists {
			// Should never happen since only block types in the schema would be
			// put in blocks list
			continue
		}

		switch blockS.Nesting {
		case configschema.NestingSingle, configschema.NestingGroup:
			ret[typeName] = marshalExpressions(block.Body, &blockS.Block)
		case configschema.NestingList, configschema.NestingSet:
			if _, exists := ret[typeName]; !exists {
				ret[typeName] = make([]map[string]any, 0, 1)
			}
			ret[typeName] = append(ret[typeName].([]map[string]any), marshalExpressions(block.Body, &blockS.Block))
		case configschema.NestingMap:
			if _, exists := ret[typeName]; !exists {
				ret[typeName] = make(map[string]map[string]any)
			}
			// NestingMap blocks always have the key in the first (and only) label
			key := block.Labels[0]
			retMap := ret[typeName].(map[string]map[string]any)
			retMap[key] = marshalExpressions(block.Body, &blockS.Block)
		}
	}

	return ret
}
