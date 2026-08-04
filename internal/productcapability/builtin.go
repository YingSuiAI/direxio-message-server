package productcapability

// RegisterBuiltinCapabilities registers all built-in Product capabilities
func RegisterBuiltinCapabilities(registry *Registry, db interface{}) error {
	// Register echo capability (test)
	if err := registerEchoCapability(registry); err != nil {
		return err
	}

	// Register real Product capabilities
	// contacts := NewContactsCapability(db.(*sql.DB))
	// registry.Register(contacts)

	// TODO: Register rooms, messages, members, channels capabilities

	return nil
}
