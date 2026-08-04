package productcapability

import "fmt"

// RegisterBuiltinCapabilities is intentionally retired.  Product capabilities
// are now built only by NewRegistryWithInvokerChecked, which binds every
// descriptor to ProductCore/Matrix and validates canonical schemas and scope
// metadata before publication.  Keeping a compatibility symbol lets old
// integrations fail explicitly instead of silently publishing the historical
// test echo or incomplete direct-SQL descriptors.
func RegisterBuiltinCapabilities(registry *Registry, _ interface{}) error {
	if registry == nil {
		return fmt.Errorf("registry is required")
	}
	return fmt.Errorf("legacy builtin Product capabilities are disabled; use NewRegistryWithInvokerChecked")
}
