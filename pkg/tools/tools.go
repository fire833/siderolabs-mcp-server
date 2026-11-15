// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package tools holds helpers shared by the MCP tool sets.
package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cosi-project/runtime/pkg/resource"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siderolabs/omni/client/pkg/cosi/labels"
	"github.com/siderolabs/omni/client/pkg/omnictl/resources"
	yaml "go.yaml.in/yaml/v4"
)

// ResourceQuery selects COSI resources, as `omnictl get`/`talosctl get` do.
//
// Tools describe type and selector with their own examples, see [InputSchema].
type ResourceQuery struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty" jsonschema:"resource ID; omit to list all resources of the type"`
	Namespace string `json:"namespace,omitempty" jsonschema:"resource namespace; defaults to the type's default namespace"`
	Selector  string `json:"selector,omitempty"`
}

// InputSchema infers the input schema of In and lets patch adjust its properties.
func InputSchema[In any](patch func(props map[string]*jsonschema.Schema)) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}

	patch(s.Properties)

	return s
}

// Tool pairs a tool definition with its typed handler.
type Tool[In any] struct {
	Def     *mcp.Tool
	Handler mcp.ToolHandlerFor[In, any]
}

// Add registers the tool on the server.
func Add[In any](server *mcp.Server, t Tool[In]) {
	mcp.AddTool(server, t.Def, t.Handler)
}

// Text wraps a string into a tool result.
func Text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// GetResources gets or lists COSI resources matching the query and renders them as YAML.
func GetResources(ctx context.Context, st state.State, in ResourceQuery) (string, error) {
	rd, err := resources.ResolveType(ctx, st, in.Type)
	if err != nil {
		return "", err
	}

	ns := in.Namespace
	if ns == "" {
		ns = rd.TypedSpec().DefaultNamespace
	}

	md := resource.NewMetadata(ns, rd.TypedSpec().Type, in.ID, resource.VersionUndefined)

	var items []resource.Resource

	if in.ID != "" {
		var r resource.Resource

		if r, err = st.Get(ctx, md); err != nil {
			return "", err
		}

		items = append(items, r)
	} else {
		var opts []state.ListOption

		if in.Selector != "" {
			var q *resource.LabelQuery

			if q, err = labels.ParseQuery(in.Selector); err != nil {
				return "", fmt.Errorf("invalid selector: %w", err)
			}

			opts = append(opts, state.WithLabelQuery(resource.RawLabelQuery(*q)))
		}

		var list resource.List

		if list, err = st.List(ctx, md, opts...); err != nil {
			return "", err
		}

		items = list.Items
	}

	// closing an encoder which has not written any document fails
	if len(items) == 0 {
		return "", nil
	}

	var sb strings.Builder

	enc := yaml.NewEncoder(&sb)

	for _, r := range items {
		var out any

		if out, err = resource.MarshalYAML(r); err != nil {
			return "", err
		}

		if err = enc.Encode(out); err != nil {
			return "", err
		}
	}

	if err = enc.Close(); err != nil {
		return "", err
	}

	return sb.String(), nil
}
