// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package jsonconfig

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hcltest"
	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/configs/configschema"
)

func TestMarshalExpressions(t *testing.T) {
	tests := []struct {
		Input hcl.Body
		Want  expressions
	}{
		{
			&hclsyntax.Body{
				Attributes: hclsyntax.Attributes{
					"foo": &hclsyntax.Attribute{
						Expr: &hclsyntax.LiteralValueExpr{
							Val: cty.StringVal("bar"),
						},
					},
				},
			},
			expressions{
				"foo": expression{
					ConstantValue: json.RawMessage([]byte(`"bar"`)),
					References:    []string(nil),
				},
			},
		},
		{
			hcltest.MockBody(&hcl.BodyContent{
				Attributes: hcl.Attributes{
					"foo": {
						Name: "foo",
						Expr: hcltest.MockExprTraversalSrc(`var.list[1]`),
					},
				},
			}),
			expressions{
				"foo": expression{
					References: []string{"var.list[1]", "var.list"},
				},
			},
		},
		{
			hcltest.MockBody(&hcl.BodyContent{
				Attributes: hcl.Attributes{
					"foo": {
						Name: "foo",
						Expr: hcltest.MockExprTraversalSrc(`data.template_file.foo[1].vars["baz"]`),
					},
				},
			}),
			expressions{
				"foo": expression{
					References: []string{"data.template_file.foo[1].vars[\"baz\"]", "data.template_file.foo[1].vars", "data.template_file.foo[1]", "data.template_file.foo"},
				},
			},
		},
		{
			hcltest.MockBody(&hcl.BodyContent{
				Attributes: hcl.Attributes{
					"foo": {
						Name: "foo",
						Expr: hcltest.MockExprTraversalSrc(`module.foo.bar`),
					},
				},
			}),
			expressions{
				"foo": expression{
					References: []string{"module.foo.bar", "module.foo"},
				},
			},
		},
		{
			hcltest.MockBody(&hcl.BodyContent{
				Blocks: hcl.Blocks{
					{
						Type: "block_to_attr",
						Body: hcltest.MockBody(&hcl.BodyContent{

							Attributes: hcl.Attributes{
								"foo": {
									Name: "foo",
									Expr: hcltest.MockExprTraversalSrc(`module.foo.bar`),
								},
							},
						}),
					},
				},
			}),
			expressions{
				"block_to_attr": expression{
					References: []string{"module.foo.bar", "module.foo"},
				},
			},
		},
	}

	for _, test := range tests {
		schema := &configschema.Block{
			Attributes: map[string]*configschema.Attribute{
				"foo": {
					Type:     cty.String,
					Optional: true,
				},
				"block_to_attr": {
					Type: cty.List(cty.Object(map[string]cty.Type{
						"foo": cty.String,
					})),
				},
			},
		}

		got := marshalExpressions(test.Input, schema)
		if !reflect.DeepEqual(got, test.Want) {
			t.Errorf("wrong result:\nGot: %#v\nWant: %#v\n", got, test.Want)
		}
	}
}

func TestMarshalExpressions_singleModuleMode(t *testing.T) {
	// In single-module mode the given schema is nil, which should
	// cause the result to always be nil. Refer to the docs on
	// [inSingleExpressionMode] for more information.
	input := hcltest.MockBody(&hcl.BodyContent{
		Attributes: hcl.Attributes{
			"foo": {
				Name: "foo",
				Expr: hcltest.MockExprTraversalSrc(`var.list[1]`),
			},
		},
	})
	got := marshalExpressions(input, nil)
	if got != nil {
		t.Errorf("wrong result:\nGot: %#v\nWant: <nil>", got)
	}
}

func TestMarshalExpression(t *testing.T) {
	tests := []struct {
		Input hcl.Expression
		Want  expression
	}{
		{
			nil,
			expression{},
		},
	}

	for _, test := range tests {
		got := marshalExpression(test.Input)
		if !reflect.DeepEqual(got, test.Want) {
			t.Fatalf("wrong result:\nGot: %#v\nWant: %#v\n", got, test.Want)
		}
	}
}
