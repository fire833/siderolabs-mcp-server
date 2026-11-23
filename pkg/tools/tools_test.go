// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cosi-project/runtime/pkg/resource/meta"
	"github.com/cosi-project/runtime/pkg/state"
	"github.com/cosi-project/runtime/pkg/state/impl/inmem"

	"github.com/siderolabs/mcp-server/pkg/tools"
)

// NewState returns an in-memory state holding the ResourceDefinition of ResourceDefinitions, labeled a=b,
// so that "resourcedefinitions" resolves and lists itself.
func NewState(t *testing.T) state.State {
	t.Helper()

	st := state.WrapCore(inmem.NewState())

	rd, err := meta.NewResourceDefinition((&meta.ResourceDefinition{}).ResourceDefinition())
	if err != nil {
		t.Fatal(err)
	}

	rd.Metadata().Labels().Set("a", "b")

	if err = st.Create(t.Context(), rd); err != nil {
		t.Fatal(err)
	}

	return st
}

func TestGetResources(t *testing.T) {
	st := NewState(t)
	id := "resourcedefinitions.meta.cosi.dev"

	for _, tc := range []struct {
		name    string
		in      tools.ResourceQuery
		want    string
		wantErr bool
	}{
		{name: "list", in: tools.ResourceQuery{Type: "resourcedefinitions"}, want: "id: " + id},
		{name: "get", in: tools.ResourceQuery{Type: "resourcedefinitions", ID: id}, want: "id: " + id},
		{name: "selector match", in: tools.ResourceQuery{Type: "resourcedefinitions", Selector: "a=b"}, want: "id: " + id},
		{name: "selector miss", in: tools.ResourceQuery{Type: "resourcedefinitions", Selector: "a=c"}},
		{name: "empty namespace", in: tools.ResourceQuery{Type: "resourcedefinitions", Namespace: "other"}},
		{name: "bad selector", in: tools.ResourceQuery{Type: "resourcedefinitions", Selector: "a in (b"}, wantErr: true},
		{name: "missing id", in: tools.ResourceQuery{Type: "resourcedefinitions", ID: "nope"}, wantErr: true},
		{name: "unknown type", in: tools.ResourceQuery{Type: "nope"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := tools.GetResources(context.Background(), st, tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}

			if !strings.Contains(out, tc.want) || (tc.want == "" && out != "") {
				t.Errorf("out = %q, want containing %q", out, tc.want)
			}
		})
	}
}
