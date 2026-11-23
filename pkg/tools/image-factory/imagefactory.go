// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package imagefactory implements Image Factory tools.
package imagefactory

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/siderolabs/image-factory/pkg/client"
	"github.com/siderolabs/image-factory/pkg/schematic"

	"github.com/siderolabs/mcp-server/pkg/tools"
)

type talosVersionInput struct {
	TalosVersion string `json:"talos_version" jsonschema:"Talos version, e.g. v1.11.0"`
}

type schematicCreateInput struct {
	Schematic string `json:"schematic" jsonschema:"schematic YAML (customization.systemExtensions, extraKernelArgs, overlay, ...)"`
}

type schematicGetInput struct {
	ID string `json:"id" jsonschema:"schematic ID"`
}

type artifactInput struct {
	Schematic    string `json:"schematic" jsonschema:"schematic ID"`
	TalosVersion string `json:"talos_version" jsonschema:"Talos version, e.g. v1.11.0"`
	Arch         string `json:"arch" jsonschema:"architecture"`
}

type scanReportInput struct {
	artifactInput
	Format string `json:"format,omitempty" jsonschema:"report format: table (plain text), json, sarif or cdx (CycloneDX)"`
}

// archSchema restricts arch to the architectures Image Factory builds.
func archSchema(props map[string]*jsonschema.Schema) {
	props["arch"].Enum = []any{"amd64", "arm64"}
}

// Edition names of Image Factory, as announced in its Server header.
const (
	editionStandard   = "Image Factory"
	editionEnterprise = "Image Factory Enterprise"
)

type factoryInfo struct {
	Edition string `json:"edition"`
	Version string `json:"version"`
}

// probe identifies the factory at baseURL from the Server header it sets on every API response,
// e.g. "Image Factory Enterprise v1.8.0".
func probe(ctx context.Context, baseURL string) (factoryInfo, error) {
	u, err := url.JoinPath(baseURL, "versions")
	if err != nil {
		return factoryInfo{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return factoryInfo{}, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return factoryInfo{}, err
	}

	resp.Body.Close() //nolint:errcheck

	server := resp.Header.Get("Server")

	name, version, _ := strings.Cut(server, " v")

	switch name {
	case editionStandard:
		return factoryInfo{Edition: "standard", Version: "v" + version}, nil
	case editionEnterprise:
		return factoryInfo{Edition: "enterprise", Version: "v" + version}, nil
	default:
		return factoryInfo{}, fmt.Errorf("unrecognized Image Factory Server header %q", server)
	}
}

func jsonText(v any) (*mcp.CallToolResult, any, error) {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}

	return tools.Text(string(out)), nil, nil
}

// Register adds the Image Factory tools for the factory at baseURL to the server.
//
// IMAGE_FACTORY_TOKEN, if set, is sent as a bearer token. The SBOM, scan and VEX tools are added
// only when the factory identifies itself as Image Factory Enterprise.
func Register(ctx context.Context, server *mcp.Server, baseURL string) error {
	var opts []client.Option

	if token := os.Getenv("IMAGE_FACTORY_TOKEN"); token != "" {
		opts = append(opts, client.WithBearerToken(token))
	}

	c, err := client.New(baseURL, opts...)
	if err != nil {
		return fmt.Errorf("failed to create image factory client: %w", err)
	}

	tools.Add(server, factoryInfoTool(baseURL))
	tools.Add(server, factoryVersions(c))
	tools.Add(server, factoryExtensions(c))
	tools.Add(server, factorySchematicGet(c))
	tools.Add(server, factorySchematicCreate(c))

	info, err := probe(ctx, baseURL)
	if err != nil {
		log.Printf("failed to identify Image Factory (%v), Enterprise tools disabled", err)

		return nil
	}

	if info.Edition != "enterprise" {
		return nil
	}

	tools.Add(server, factorySPDX(c))
	tools.Add(server, factoryScanReport(c))
	tools.Add(server, factoryVEX(c))

	return nil
}

func factoryInfoTool(baseURL string) tools.Tool[struct{}] {
	return tools.Tool[struct{}]{
		Def: &mcp.Tool{
			Name:        "factory_info",
			Description: "Get the Image Factory edition (standard or enterprise) and version.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			info, err := probe(ctx, baseURL)
			if err != nil {
				return nil, nil, err
			}

			return jsonText(info)
		},
	}
}

func factoryVersions(c *client.Client) tools.Tool[struct{}] {
	return tools.Tool[struct{}]{
		Def: &mcp.Tool{
			Name:        "factory_versions",
			Description: "List Talos versions available in Image Factory.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			v, err := c.Versions(ctx)
			if err != nil {
				return nil, nil, err
			}

			return jsonText(v)
		},
	}
}

func factoryExtensions(c *client.Client) tools.Tool[talosVersionInput] {
	return tools.Tool[talosVersionInput]{
		Def: &mcp.Tool{
			Name:        "factory_extensions",
			Description: "List system extensions and overlays available in Image Factory for a Talos version.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in talosVersionInput) (*mcp.CallToolResult, any, error) {
			ext, err := c.ExtensionsVersions(ctx, in.TalosVersion)
			if err != nil {
				return nil, nil, err
			}

			overlays, err := c.OverlaysVersions(ctx, in.TalosVersion)
			if err != nil {
				return nil, nil, err
			}

			return jsonText(map[string]any{"extensions": ext, "overlays": overlays})
		},
	}
}

func factorySchematicGet(c *client.Client) tools.Tool[schematicGetInput] {
	return tools.Tool[schematicGetInput]{
		Def: &mcp.Tool{
			Name:        "factory_schematic_get",
			Description: "Get a schematic from Image Factory by ID.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in schematicGetInput) (*mcp.CallToolResult, any, error) {
			s, err := c.SchematicGet(ctx, in.ID)
			if err != nil {
				return nil, nil, err
			}

			out, err := s.Marshal()
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(string(out)), nil, nil
		},
	}
}

// Schematics are content-addressed, so creating one is idempotent.
func factorySchematicCreate(c *client.Client) tools.Tool[schematicCreateInput] {
	return tools.Tool[schematicCreateInput]{
		Def: &mcp.Tool{
			Name:        "factory_schematic_create",
			Description: "Upload a schematic to Image Factory and return its ID along with the installer image and download base URL.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    false,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in schematicCreateInput) (*mcp.CallToolResult, any, error) {
			s, err := schematic.Unmarshal([]byte(in.Schematic))
			if err != nil {
				return nil, nil, err
			}

			id, _, err := c.SchematicCreate(ctx, *s)
			if err != nil {
				return nil, nil, err
			}

			u, err := url.Parse(c.BaseURL())
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(fmt.Sprintf("id: %s\ninstaller: %s/installer/%s:<talos_version>\ndownloads: %s/image/%s/<talos_version>/\n",
				id, u.Host, id, c.BaseURL(), id)), nil, nil
		},
	}
}

func factorySPDX(c *client.Client) tools.Tool[artifactInput] {
	return tools.Tool[artifactInput]{
		Def: &mcp.Tool{
			Name:        "factory_spdx",
			Description: "Get the SPDX SBOM bundle (JSON) for a schematic, Talos version and architecture. Image Factory Enterprise only.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
			InputSchema: tools.InputSchema[artifactInput](archSchema),
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in artifactInput) (*mcp.CallToolResult, any, error) {
			out, err := c.SPDXBundle(ctx, in.Schematic, in.TalosVersion, in.Arch)
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(string(out)), nil, nil
		},
	}
}

func factoryScanReport(c *client.Client) tools.Tool[scanReportInput] {
	return tools.Tool[scanReportInput]{
		Def: &mcp.Tool{
			Name:        "factory_scan_report",
			Description: "Get the vulnerability scan report for a schematic, Talos version and architecture. Image Factory Enterprise only.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
			InputSchema: tools.InputSchema[scanReportInput](func(props map[string]*jsonschema.Schema) {
				archSchema(props)
				props["format"].Enum = []any{"table", "json", "sarif", "cdx"}
				props["format"].Default = json.RawMessage(`"table"`)
			}),
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in scanReportInput) (*mcp.CallToolResult, any, error) {
			// the report filename extension selects the format
			out, err := c.ScanReport(ctx, in.Schematic, in.TalosVersion, in.Arch, "report."+in.Format)
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(string(out)), nil, nil
		},
	}
}

func factoryVEX(c *client.Client) tools.Tool[talosVersionInput] {
	return tools.Tool[talosVersionInput]{
		Def: &mcp.Tool{
			Name:        "factory_vex",
			Description: "Get the VEX document (JSON) for a Talos version. Image Factory Enterprise only.",
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:    true,
				DestructiveHint: new(false),
				IdempotentHint:  true,
				OpenWorldHint:   new(true),
			},
		},
		Handler: func(ctx context.Context, _ *mcp.CallToolRequest, in talosVersionInput) (*mcp.CallToolResult, any, error) {
			out, err := c.VEXDocument(ctx, in.TalosVersion)
			if err != nil {
				return nil, nil, err
			}

			return tools.Text(string(out)), nil, nil
		},
	}
}
