// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package omni implements Omni tools.
package omni

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/state"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/client/management"
	"github.com/siderolabs/omni/client/pkg/template/operations"

	"github.com/siderolabs/mcp-server/pkg/tools"
)

// NewClient creates an Omni client from OMNI_ENDPOINT and OMNI_SERVICE_ACCOUNT_KEY.
//
// It returns a nil client when they are not set.
func NewClient() (*client.Client, error) {
	endpoint := os.Getenv("OMNI_ENDPOINT")
	key := os.Getenv("OMNI_SERVICE_ACCOUNT_KEY")

	if endpoint == "" || key == "" {
		log.Print("OMNI_ENDPOINT/OMNI_SERVICE_ACCOUNT_KEY not set, Omni access disabled")

		return nil, nil //nolint:nilnil
	}

	c, err := client.New(endpoint, client.WithServiceAccount(key))
	if err != nil {
		return nil, fmt.Errorf("failed to create omni client: %w", err)
	}

	return c, nil
}

type clusterInput struct {
	Cluster string `json:"cluster" jsonschema:"cluster name"`
}

type kubeconfigInput struct {
	Cluster string `json:"cluster" jsonschema:"cluster name"`
	TTL     string `json:"ttl,omitempty" jsonschema:"service account token TTL as Go duration, default 1h"`
}

type templateInput struct {
	Template string `json:"template" jsonschema:"cluster template YAML (multi-document)"`
	DryRun   bool   `json:"dry_run" jsonschema:"true only reports the changes; false applies them to the cluster"`
}

// Register adds the Omni tools to the server.
func Register(server *mcp.Server, c *client.Client) {
	st := c.Omni().State()

	tools.Add(server, omniGet(st))
	tools.Add(server, omniTalosconfig(c))
	tools.Add(server, omniKubeconfig(c))
	tools.Add(server, omniTemplateSync(st))
}

func omniGet(st state.State) tools.Tool[tools.ResourceQuery] {
	return tools.Tool[tools.ResourceQuery]{
		Def: &mcp.Tool{
			Name: "omni_get",
			Description: "Get or list Omni COSI resources as YAML, equivalent to `omnictl get`. " +
				"Use type 'resourcedefinitions' to discover available types. " +
				"Output is unredacted and may contain credentials, e.g. join tokens embedded in machine status.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(false),
			},
			InputSchema: tools.InputSchema[tools.ResourceQuery](func(props map[string]*jsonschema.Schema) {
				props["type"].Description = "resource type or alias as accepted by omnictl get, e.g. clusters, machinestatus"
				props["selector"].Description = "label selector for list, e.g. omni.sidero.dev/cluster=foo"
			}),
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in tools.ResourceQuery) (*mcp.CallToolResult, any, error) {
			out, err := tools.GetResources(ctx, st, in)
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(out), nil, nil
		},
	}
}

func omniTalosconfig(c *client.Client) tools.Tool[clusterInput] {
	return tools.Tool[clusterInput]{
		Def: &mcp.Tool{
			Name:        "omni_talosconfig",
			Description: "Get talosconfig for a cluster managed by Omni. The output is a credential granting Talos API access to the cluster nodes.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(false),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in clusterInput) (*mcp.CallToolResult, any, error) {
			cfg, err := c.Management().WithCluster(in.Cluster).Talosconfig(ctx)
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(string(cfg)), nil, nil
		},
	}
}

func omniKubeconfig(c *client.Client) tools.Tool[kubeconfigInput] {
	return tools.Tool[kubeconfigInput]{
		Def: &mcp.Tool{
			Name: "omni_kubeconfig",
			Description: "Get a service-account kubeconfig for a cluster managed by Omni. " +
				"The output is a cluster-admin (system:masters) credential valid for ttl.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				DestructiveHint: new(false),
				IdempotentHint:  false,
				OpenWorldHint:   new(false),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in kubeconfigInput) (*mcp.CallToolResult, any, error) {
			ttl := time.Hour

			if in.TTL != "" {
				var err error

				if ttl, err = time.ParseDuration(in.TTL); err != nil {
					return nil, nil, fmt.Errorf("invalid ttl: %w", err)
				}
			}

			cfg, err := c.Management().WithCluster(in.Cluster).Kubeconfig(ctx,
				management.WithServiceAccount(ttl, "siderolabs-mcp-server", "system:masters"),
			)
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(string(cfg)), nil, nil
		},
	}
}

func omniTemplateSync(st state.State) tools.Tool[templateInput] {
	return tools.Tool[templateInput]{
		Def: &mcp.Tool{
			Name: "omni_template_sync",
			Description: "Create or update a cluster from an Omni cluster template, equivalent to `omnictl cluster template sync`. " +
				"Run with dry_run true first to preview the changes; dry_run false applies them.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				DestructiveHint: new(true),
				IdempotentHint:  true,
				OpenWorldHint:   new(false),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in templateInput) (*mcp.CallToolResult, any, error) {
			out, err := syncTemplate(ctx, st, in)
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(out), nil, nil
		},
	}
}

func syncTemplate(ctx context.Context, st state.State, in templateInput) (string, error) {
	// confine template file references (patches etc.) to the working directory
	root, err := os.OpenRoot(".")
	if err != nil {
		return "", err
	}

	defer root.Close() //nolint:errcheck

	var sb strings.Builder

	err = operations.SyncTemplate(ctx, strings.NewReader(in.Template), &sb, st, operations.SyncOptions{DryRun: in.DryRun, Verbose: true}, root)

	return sb.String(), err
}
