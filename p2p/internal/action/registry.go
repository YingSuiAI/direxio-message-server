package action

import (
	"fmt"
	"sort"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

// Registry combines independently owned action handler maps and validates that
// they exactly cover the generated ProductCore action specifications.
type Registry struct {
	specs        []serviceapi.ActionSpec
	specByName   map[string]serviceapi.ActionSpec
	routeSpecial map[string]struct{}
	handlers     map[string]Handler
	owners       map[string]string
}

// NewRegistry creates an empty registry for specs. routeSpecial actions are
// implemented by a protocol adapter rather than Service.Handle and are the only
// specs allowed to omit a handler.
func NewRegistry(specs []serviceapi.ActionSpec, routeSpecial ...string) (*Registry, error) {
	registry := &Registry{
		specs:        append([]serviceapi.ActionSpec{}, specs...),
		specByName:   make(map[string]serviceapi.ActionSpec, len(specs)),
		routeSpecial: make(map[string]struct{}, len(routeSpecial)),
		handlers:     make(map[string]Handler, len(specs)),
		owners:       make(map[string]string, len(specs)),
	}
	for _, spec := range specs {
		if spec.Name == "" || strings.TrimSpace(spec.Name) != spec.Name {
			return nil, fmt.Errorf("action spec name %q is not canonical", spec.Name)
		}
		if _, exists := registry.specByName[spec.Name]; exists {
			return nil, fmt.Errorf("duplicate action spec %q", spec.Name)
		}
		registry.specByName[spec.Name] = spec
	}
	for _, name := range routeSpecial {
		if name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("route-special action name %q is not canonical", name)
		}
		if _, exists := registry.specByName[name]; !exists {
			return nil, fmt.Errorf("route-special action %q has no action spec", name)
		}
		if _, exists := registry.routeSpecial[name]; exists {
			return nil, fmt.Errorf("duplicate route-special action %q", name)
		}
		registry.routeSpecial[name] = struct{}{}
	}
	return registry, nil
}

// Merge atomically adds a module's handlers. An invalid module leaves the
// registry unchanged.
func (r *Registry) Merge(module string, handlers map[string]Handler) error {
	module = strings.TrimSpace(module)
	if module == "" {
		return fmt.Errorf("action handler module name is required")
	}
	names := make([]string, 0, len(handlers))
	for name := range handlers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		handler := handlers[name]
		if name == "" || strings.TrimSpace(name) != name {
			return fmt.Errorf("action handler name %q in module %q is not canonical", name, module)
		}
		if handler == nil {
			return fmt.Errorf("nil handler for action %q in module %q", name, module)
		}
		if _, exists := r.specByName[name]; !exists {
			return fmt.Errorf("handler for action %q in module %q has no action spec", name, module)
		}
		if _, special := r.routeSpecial[name]; special {
			return fmt.Errorf("route-special action %q must not register a service handler", name)
		}
		if owner, exists := r.owners[name]; exists {
			return fmt.Errorf("duplicate handler for action %q in modules %q and %q", name, owner, module)
		}
	}
	for _, name := range names {
		r.handlers[name] = handlers[name]
		r.owners[name] = module
	}
	return nil
}

// Validate checks exact handler coverage for the published action contract.
func (r *Registry) Validate() error {
	for _, spec := range r.specs {
		if _, special := r.routeSpecial[spec.Name]; special {
			continue
		}
		if _, exists := r.handlers[spec.Name]; !exists {
			return fmt.Errorf("missing handler for action %q", spec.Name)
		}
	}
	return nil
}

// Handlers returns a copy so callers cannot bypass duplicate or contract
// validation after construction.
func (r *Registry) Handlers() map[string]Handler {
	handlers := make(map[string]Handler, len(r.handlers))
	for name, handler := range r.handlers {
		handlers[name] = handler
	}
	return handlers
}
