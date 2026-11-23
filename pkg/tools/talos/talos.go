// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package talos implements Talos tools, reaching nodes either directly or through Omni.
package talos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/siderolabs/mcp-server/pkg/tools"
)

// NewClient creates a Talos client from the default talosconfig, optionally for a named context.
//
// It returns a nil client when no usable talosconfig is found.
func NewClient(ctx context.Context, contextName string) *talosclient.Client {
	opts := []talosclient.OptionFunc{talosclient.WithDefaultConfig()}

	if contextName != "" {
		opts = append(opts, talosclient.WithContextName(contextName))
	}

	c, err := talosclient.New(ctx, opts...)
	if err != nil {
		log.Printf("no usable talosconfig (%v), direct Talos access disabled", err)

		return nil
	}

	return c
}

// Backends holds the available routes to Talos API; nil means disabled.
type Backends struct {
	// Omni routes requests through the Omni Talos API proxy.
	Omni *client.Client
	// Direct uses the local talosconfig.
	Direct *talosclient.Client
}

type backends struct {
	omni   *omniTalos
	direct *talosclient.Client
}

// omniTalos reaches Talos nodes through the Omni Talos API proxy.
type omniTalos struct {
	c   *client.Client
	cfg *clientconfig.Config
	key string
	mu  sync.Mutex
}

// config fetches the instance talosconfig from Omni once; failures are retried on the next call.
func (o *omniTalos) config(ctx context.Context) (*clientconfig.Config, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.cfg != nil {
		return o.cfg, nil
	}

	raw, err := o.c.Management().Talosconfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get talosconfig from Omni: %w", err)
	}

	if o.cfg, err = clientconfig.FromBytes(raw); err != nil {
		return nil, err
	}

	return o.cfg, nil
}

// client returns a Talos client for the target and a func to release it.
func (b backends) client(ctx context.Context, cluster string) (*talosclient.Client, func(), error) {
	if cluster == "" {
		if b.direct == nil {
			return nil, nil, errors.New("direct Talos access is disabled; set cluster to route through Omni")
		}

		return b.direct, func() {}, nil
	}

	if b.omni == nil {
		return nil, nil, errors.New("Omni access is disabled; omit cluster to use talosconfig directly") //nolint:staticcheck
	}

	cfg, err := b.omni.config(ctx)
	if err != nil {
		return nil, nil, err
	}

	// ponytail: new gRPC connection per call, cache per cluster if latency matters
	c, err := talosclient.New(ctx,
		talosclient.WithConfig(cfg),
		talosclient.WithServiceAccount(b.omni.key),
		talosclient.WithCluster(cluster),
	)
	if err != nil {
		return nil, nil, err
	}

	return c, func() { c.Close() }, nil //nolint:errcheck
}

type talosTarget struct {
	Cluster string   `json:"cluster,omitempty" jsonschema:"Omni cluster name: routes the request through Omni; omit to use the local talosconfig directly"`
	Nodes   []string `json:"nodes" jsonschema:"target nodes: IPs/hostnames, or Omni machine IDs/node names when routed through Omni"`
}

type talosGetInput struct {
	tools.ResourceQuery
	talosTarget
}

type talosLogsInput struct {
	Service string `json:"service" jsonschema:"service ID, e.g. kubelet, etcd, apid, machined"`
	talosTarget
	TailLines int `json:"tail_lines,omitempty" jsonschema:"number of lines from the end"`
}

type talosDmesgInput struct {
	talosTarget
	TailLines int `json:"tail_lines,omitempty" jsonschema:"number of lines from the end"`
}

const maxTailLines = 10000

// tailLines bounds tail_lines to a meaningful range; the default is applied by the SDK.
func tailLines(props map[string]*jsonschema.Schema) {
	p := props["tail_lines"]
	p.Default = json.RawMessage("200")
	p.Minimum = new(1.0)
	p.Maximum = new(float64(maxTailLines))
}

// targetSchema fixes the inferred schema of talosTarget.Nodes.
func targetSchema(props map[string]*jsonschema.Schema) {
	// a nil slice infers as ["null", "array"]
	props["nodes"].Types = nil
	props["nodes"].Type = "array"
	props["nodes"].MinItems = new(1)
	props["nodes"].Items.MinLength = new(1)
}

// eachNode runs fn against every target node and concatenates the per-node output; node errors are reported inline.
func (b backends) eachNode(
	ctx context.Context, t talosTarget,
	fn func(ctx context.Context, c *talosclient.Client) (string, error),
) (*mcp.CallToolResult, any, error) {
	if len(t.Nodes) == 0 {
		return nil, nil, errors.New("at least one node is required")
	}

	c, release, err := b.client(ctx, t.Cluster)
	if err != nil {
		return nil, nil, err
	}

	defer release()

	var sb strings.Builder

	for _, node := range t.Nodes {
		out, err := fn(talosclient.WithNode(ctx, node), c)
		if err != nil {
			out = "error: " + err.Error() + "\n"
		}

		fmt.Fprintf(&sb, "--- node %s ---\n%s", node, out)
	}

	return tools.Text(sb.String()), nil, nil
}

// Register adds the Talos tools to the server.
func Register(server *mcp.Server, routes Backends) {
	b := backends{direct: routes.Direct}

	if routes.Omni != nil {
		// same service account as the Omni client, see omni.NewClient
		b.omni = &omniTalos{c: routes.Omni, key: os.Getenv("OMNI_SERVICE_ACCOUNT_KEY")}
	}

	tools.Add(server, talosGet(b))
	tools.Add(server, talosLogs(b))
	tools.Add(server, talosDmesg(b))
	tools.Add(server, talosVersion(b))
}

func talosGet(b backends) tools.Tool[talosGetInput] {
	return tools.Tool[talosGetInput]{
		Def: &mcp.Tool{
			Name:        "talos_get",
			Description: "Get or list Talos COSI resources as YAML on each node, equivalent to `talosctl get`. Use type 'rd' to discover available types.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(false),
			},
			InputSchema: tools.InputSchema[talosGetInput](func(props map[string]*jsonschema.Schema) {
				targetSchema(props)
				props["type"].Description = "resource type or alias as accepted by talosctl get, e.g. members, service, machineconfig"
				props["selector"].Description = "label selector for list, e.g. key=value"
			}),
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in talosGetInput) (*mcp.CallToolResult, any, error) {
			return b.eachNode(ctx, in.talosTarget, func(ctx context.Context, c *talosclient.Client) (string, error) {
				return tools.GetResources(ctx, c.State(), in.ResourceQuery)
			})
		},
	}
}

func talosLogs(b backends) tools.Tool[talosLogsInput] {
	return tools.Tool[talosLogsInput]{
		Def: &mcp.Tool{
			Name:        "talos_logs",
			Description: "Get service logs from each node, equivalent to `talosctl logs`.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(false),
			},
			InputSchema: tools.InputSchema[talosLogsInput](func(props map[string]*jsonschema.Schema) {
				targetSchema(props)
				tailLines(props)
			}),
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in talosLogsInput) (*mcp.CallToolResult, any, error) {
			return b.eachNode(ctx, in.talosTarget, func(ctx context.Context, c *talosclient.Client) (string, error) {
				stream, err := c.Logs(ctx, constants.SystemContainerdNamespace, common.ContainerDriver_CONTAINERD, in.Service, false, int32(in.TailLines))
				if err != nil {
					return "", err
				}

				return readStream(stream.Recv)
			})
		},
	}
}

func talosDmesg(b backends) tools.Tool[talosDmesgInput] {
	return tools.Tool[talosDmesgInput]{
		Def: &mcp.Tool{
			Name:        "talos_dmesg",
			Description: "Get kernel log from each node, equivalent to `talosctl dmesg`.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(false),
			},
			InputSchema: tools.InputSchema[talosDmesgInput](func(props map[string]*jsonschema.Schema) {
				targetSchema(props)
				tailLines(props)
			}),
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in talosDmesgInput) (*mcp.CallToolResult, any, error) {
			return b.eachNode(ctx, in.talosTarget, func(ctx context.Context, c *talosclient.Client) (string, error) {
				stream, err := c.Dmesg(ctx, false, false)
				if err != nil {
					return "", err
				}

				out, err := readStream(stream.Recv)
				if err != nil {
					return "", err
				}

				lines := strings.SplitAfter(out, "\n")

				return strings.Join(lines[max(0, len(lines)-in.TailLines):], ""), nil
			})
		},
	}
}

func talosVersion(b backends) tools.Tool[talosTarget] {
	return tools.Tool[talosTarget]{
		Def: &mcp.Tool{
			Name:        "talos_version",
			Description: "Get Talos version information from each node, equivalent to `talosctl version`.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(false),
			},
			InputSchema: tools.InputSchema[talosTarget](targetSchema),
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in talosTarget) (*mcp.CallToolResult, any, error) {
			return b.eachNode(ctx, in, func(ctx context.Context, c *talosclient.Client) (string, error) {
				resp, err := c.Version(ctx)
				if err != nil {
					return "", err
				}

				out, err := protojson.Marshal(resp)

				return string(out) + "\n", err
			})
		},
	}
}

// readStream drains a Talos data stream into a string.
func readStream(recv func() (*common.Data, error)) (string, error) {
	var buf bytes.Buffer

	for {
		msg, err := recv()
		if errors.Is(err, io.EOF) {
			return buf.String(), nil
		}

		if err != nil {
			return buf.String(), err
		}

		buf.Write(msg.GetBytes())
	}
}
