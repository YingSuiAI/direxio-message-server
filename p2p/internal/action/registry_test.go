package action

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

func noOpHandler(context.Context, map[string]any) (any, *Error) { return nil, nil }

func actionSpec(name string, transport serviceapi.ActionTransport) serviceapi.ActionSpec {
	return serviceapi.ActionSpec{Name: name, Auth: serviceapi.ActionAuthOwner, Transport: transport}
}

func TestRegistryMergesModulesAndValidatesExactCoverage(t *testing.T) {
	specs := []serviceapi.ActionSpec{
		actionSpec("regular.action", serviceapi.ActionTransportHTTPOnly),
		actionSpec("stream.action", serviceapi.ActionTransportInternalOnly),
		actionSpec("route.action", serviceapi.ActionTransportHTTPOnly),
	}
	registry, err := NewRegistry(specs, "route.action")
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	if err := registry.Merge("regular", map[string]Handler{"regular.action": noOpHandler}); err != nil {
		t.Fatalf("Merge(regular) error = %v", err)
	}
	if err := registry.Merge("stream", map[string]Handler{"stream.action": noOpHandler}); err != nil {
		t.Fatalf("Merge(stream) error = %v", err)
	}
	if err := registry.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	handlers := registry.Handlers()
	if len(handlers) != 2 || handlers["regular.action"] == nil || handlers["stream.action"] == nil {
		t.Fatalf("Handlers() = %#v, want both non-route handlers", handlers)
	}
	if _, exists := handlers["route.action"]; exists {
		t.Fatal("route-special action unexpectedly has a service handler")
	}

	delete(handlers, "regular.action")
	if err := registry.Validate(); err != nil {
		t.Fatalf("mutating Handlers() result changed registry: %v", err)
	}
}

func TestRegistryRejectsDuplicateHandlerWithoutPartialMerge(t *testing.T) {
	specs := []serviceapi.ActionSpec{
		actionSpec("first.action", serviceapi.ActionTransportHTTPOnly),
		actionSpec("second.action", serviceapi.ActionTransportHTTPOnly),
	}
	registry, err := NewRegistry(specs)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	if err := registry.Merge("first", map[string]Handler{"first.action": noOpHandler}); err != nil {
		t.Fatalf("Merge(first) error = %v", err)
	}
	err = registry.Merge("overlap", map[string]Handler{
		"first.action":  noOpHandler,
		"second.action": noOpHandler,
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate handler") {
		t.Fatalf("Merge(overlap) error = %v, want duplicate handler", err)
	}
	if _, exists := registry.Handlers()["second.action"]; exists {
		t.Fatal("failed module merge partially installed a handler")
	}
}

func TestRegistryRejectsMissingExtraNilAndRouteHandlers(t *testing.T) {
	tests := []struct {
		name         string
		specs        []serviceapi.ActionSpec
		routeSpecial []string
		handlers     map[string]Handler
		want         string
		validate     bool
	}{
		{
			name:     "missing regular handler",
			specs:    []serviceapi.ActionSpec{actionSpec("missing.action", serviceapi.ActionTransportHTTPOnly)},
			want:     "missing handler",
			validate: true,
		},
		{
			name:     "missing stream handler",
			specs:    []serviceapi.ActionSpec{actionSpec("stream.action", serviceapi.ActionTransportInternalOnly)},
			want:     "missing handler",
			validate: true,
		},
		{
			name:     "extra handler",
			specs:    []serviceapi.ActionSpec{actionSpec("known.action", serviceapi.ActionTransportHTTPOnly)},
			handlers: map[string]Handler{"extra.action": noOpHandler},
			want:     "no action spec",
		},
		{
			name:     "nil handler",
			specs:    []serviceapi.ActionSpec{actionSpec("nil.action", serviceapi.ActionTransportHTTPOnly)},
			handlers: map[string]Handler{"nil.action": nil},
			want:     "nil handler",
		},
		{
			name:         "route-special handler",
			specs:        []serviceapi.ActionSpec{actionSpec("route.action", serviceapi.ActionTransportHTTPOnly)},
			routeSpecial: []string{"route.action"},
			handlers:     map[string]Handler{"route.action": noOpHandler},
			want:         "route-special",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistry(tt.specs, tt.routeSpecial...)
			if err != nil {
				t.Fatalf("NewRegistry() error = %v", err)
			}
			if len(tt.handlers) > 0 {
				err = registry.Merge("test", tt.handlers)
			}
			if tt.validate && err == nil {
				err = registry.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want text %q", err, tt.want)
			}
		})
	}
}

func TestNewRegistryRejectsInvalidContractInputs(t *testing.T) {
	tests := []struct {
		name         string
		specs        []serviceapi.ActionSpec
		routeSpecial []string
		want         string
	}{
		{
			name:  "duplicate specs",
			specs: []serviceapi.ActionSpec{actionSpec("same.action", serviceapi.ActionTransportHTTPOnly), actionSpec("same.action", serviceapi.ActionTransportHTTPOnly)},
			want:  "duplicate action spec",
		},
		{
			name:  "non-canonical spec",
			specs: []serviceapi.ActionSpec{actionSpec(" spaced.action ", serviceapi.ActionTransportHTTPOnly)},
			want:  "canonical",
		},
		{
			name:         "unknown route special",
			specs:        []serviceapi.ActionSpec{actionSpec("known.action", serviceapi.ActionTransportHTTPOnly)},
			routeSpecial: []string{"unknown.action"},
			want:         "no action spec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistry(tt.specs, tt.routeSpecial...)
			if err == nil || registry != nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewRegistry() = (%#v, %v), want nil and text %q", registry, err, tt.want)
			}
		})
	}
}

func TestErrorPreservesProductErrorJSON(t *testing.T) {
	raw, err := json.Marshal(&Error{Status: 409, Error: "conflict", Code: "M_CONFLICT"})
	if err != nil {
		t.Fatalf("Marshal(Error) error = %v", err)
	}
	if got, want := string(raw), `{"error":"conflict","code":"M_CONFLICT"}`; got != want {
		t.Fatalf("Marshal(Error) = %s, want %s", got, want)
	}
}

func TestErrorConstructorsPreserveStatusCodeAndMessages(t *testing.T) {
	if got := BadRequest("invalid"); got.Status != 400 || got.Error != "invalid" || got.Code != "" {
		t.Fatalf("BadRequest() = %#v", got)
	}
	if got := StatusError(409, "conflict"); got.Status != 409 || got.Error != "conflict" || got.Code != "" {
		t.Fatalf("StatusError() = %#v", got)
	}
	if got := CodedError(503, "M_UNAVAILABLE", "unavailable"); got.Status != 503 || got.Error != "unavailable" || got.Code != "M_UNAVAILABLE" {
		t.Fatalf("CodedError() = %#v", got)
	}
	if got := InternalError(fmt.Errorf("store failed")); got.Status != 500 || got.Error != "internal error: store failed" || got.Code != "" {
		t.Fatalf("InternalError() = %#v", got)
	}
}
