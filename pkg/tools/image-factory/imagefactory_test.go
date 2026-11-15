// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package imagefactory_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	imagefactory "github.com/siderolabs/mcp-server/pkg/tools/image-factory"
)

// fakeFactory serves canned Image Factory API responses, announcing itself with the given Server header.
func fakeFactory(t *testing.T, server string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", server)

		switch p := r.URL.Path; {
		case p == "/versions":
			w.Write([]byte(`["v1.11.0"]`)) //nolint:errcheck
		case p == "/version/v1.11.0/extensions/official":
			w.Write([]byte(`[{"name":"siderolabs/iscsi-tools"}]`)) //nolint:errcheck
		case p == "/version/v1.11.0/overlays/official":
			w.Write([]byte(`[{"name":"rpi_generic"}]`)) //nolint:errcheck
		case p == "/schematics" && r.Method == http.MethodPost:
			w.Write([]byte(`{"id":"abc","schematic":"customization: {}\n"}`)) //nolint:errcheck
		case p == "/schematics/abc":
			w.Write([]byte("customization:\n  extraKernelArgs: [quiet]\n")) //nolint:errcheck
		case strings.HasPrefix(p, "/spdx/"), strings.HasPrefix(p, "/scans/"), strings.HasPrefix(p, "/vex/"):
			w.Write([]byte(p)) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(srv.Close)

	return srv.URL
}

func connect(t *testing.T, baseURL string) *mcp.ClientSession {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)

	if err := imagefactory.Register(t.Context(), server, baseURL); err != nil {
		t.Fatal(err)
	}

	st, ct := mcp.NewInMemoryTransports()

	if _, err := server.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { cs.Close() }) //nolint:errcheck

	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(res.Tools))

	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}

	return names
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}

	return res.Content[0].(*mcp.TextContent).Text, res.IsError //nolint:errcheck,forcetypeassert
}

func TestEditions(t *testing.T) {
	enterprise := []string{"factory_spdx", "factory_scan_report", "factory_vex"}

	for _, tc := range []struct {
		server, edition string
		enterprise      bool
	}{
		{server: "Image Factory v1.8.0", edition: `"standard"`},
		{server: "Image Factory Enterprise v1.8.0", edition: `"enterprise"`, enterprise: true},
		{server: "nginx"},
	} {
		t.Run(tc.server, func(t *testing.T) {
			cs := connect(t, fakeFactory(t, tc.server))
			names := toolNames(t, cs)

			for _, name := range enterprise {
				if slices.Contains(names, name) != tc.enterprise {
					t.Errorf("tool %s registered = %v, want %v", name, !tc.enterprise, tc.enterprise)
				}
			}

			out, isErr := call(t, cs, "factory_info", nil)
			if tc.edition == "" {
				if !isErr {
					t.Errorf("factory_info = %q, want error", out)
				}

				return
			}

			if isErr || !strings.Contains(out, tc.edition) || !strings.Contains(out, `"v1.8.0"`) {
				t.Errorf("factory_info = %q (error %v), want edition %s", out, isErr, tc.edition)
			}
		})
	}
}

func TestTools(t *testing.T) {
	cs := connect(t, fakeFactory(t, "Image Factory Enterprise v1.8.0"))
	artifact := map[string]any{"schematic": "abc", "talos_version": "v1.11.0", "arch": "amd64"}

	for _, tc := range []struct {
		tool    string
		args    map[string]any
		want    string
		wantErr bool
	}{
		{tool: "factory_versions", want: "v1.11.0"},
		{tool: "factory_extensions", args: map[string]any{"talos_version": "v1.11.0"}, want: "rpi_generic"},
		{tool: "factory_extensions", args: map[string]any{"talos_version": "v0"}, wantErr: true},
		{tool: "factory_schematic_get", args: map[string]any{"id": "abc"}, want: "quiet"},
		{tool: "factory_schematic_get", args: map[string]any{"id": "nope"}, wantErr: true},
		{tool: "factory_schematic_create", args: map[string]any{"schematic": "customization: {}"}, want: "/installer/abc:<talos_version>"},
		{tool: "factory_schematic_create", args: map[string]any{"schematic": "bogus: 1"}, wantErr: true},
		{tool: "factory_spdx", args: artifact, want: "/spdx/abc/v1.11.0/amd64"},
		{tool: "factory_spdx", args: map[string]any{"schematic": "abc", "talos_version": "v1.11.0", "arch": "s390x"}, wantErr: true},
		{tool: "factory_scan_report", args: artifact, want: "/scans/abc/v1.11.0/amd64/report.table"},
		{tool: "factory_vex", args: map[string]any{"talos_version": "v1.11.0"}, want: "/vex/v1.11.0/vex.json"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			out, isErr := call(t, cs, tc.tool, tc.args)
			if isErr != tc.wantErr || !strings.Contains(out, tc.want) {
				t.Errorf("%s = %q (error %v), want %q (error %v)", tc.tool, out, isErr, tc.want, tc.wantErr)
			}
		})
	}
}
