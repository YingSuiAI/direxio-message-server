package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

// RegisterBuiltinCapabilities registers all built-in Product capabilities
func RegisterBuiltinCapabilities(registry *Registry, db interface{}) error {
	sqlDB, ok := db.(*sql.DB)
	if !ok {
		return fmt.Errorf("db must be *sql.DB, got %T", db)
	}

	// Register contacts capability
	contactsCapability := NewContactsCapability(sqlDB)
	if err := registry.Register(createProvider(contactsCapability)); err != nil {
		return fmt.Errorf("failed to register contacts capability: %w", err)
	}

	// Register rooms capability
	roomsCapability := NewRoomsCapability(sqlDB)
	if err := registry.Register(createRoomsProvider(roomsCapability)); err != nil {
		return fmt.Errorf("failed to register rooms capability: %w", err)
	}

	// Register messages capability
	messagesCapability := NewMessagesCapability(sqlDB)
	if err := registry.Register(createMessagesProvider(messagesCapability)); err != nil {
		return fmt.Errorf("failed to register messages capability: %w", err)
	}

	// Register members capability
	membersCapability := NewMembersCapability(sqlDB)
	if err := registry.Register(createMembersProvider(membersCapability)); err != nil {
		return fmt.Errorf("failed to register members capability: %w", err)
	}

	// Register channels capability
	channelsCapability := NewChannelsCapability(sqlDB)
	if err := registry.Register(createChannelsProvider(channelsCapability)); err != nil {
		return fmt.Errorf("failed to register channels capability: %w", err)
	}

	return nil
}

// createProvider creates a Provider for ContactsCapability
func createProvider(c *ContactsCapability) *Provider {
	return &Provider{
		Descriptor: &capv1.CapabilityDescriptor{
			CapabilityId: "product.contacts.v1",
			DisplayName:  "Contacts Management",
			Description:  "Manage user contacts with list, get, search, create, update, and delete operations",
			Readiness:    true,
			Operations: []*capv1.OperationDescriptor{
				{OperationId: "list", DisplayName: "List Contacts", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "get", DisplayName: "Get Contact", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "search", DisplayName: "Search Contacts", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "create", DisplayName: "Create Contact", OperationType: capv1.OperationType_OPERATION_TYPE_MUTATION},
				{OperationId: "update", DisplayName: "Update Contact", OperationType: capv1.OperationType_OPERATION_TYPE_MUTATION},
				{OperationId: "delete", DisplayName: "Delete Contact", OperationType: capv1.OperationType_OPERATION_TYPE_MUTATION},
			},
		},
		Handler: func(ctx context.Context, input []byte) ([]byte, error) {
			var req struct {
				Operation string          `json:"operation"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &req); err != nil {
				return nil, err
			}

			// Determine if operation is mutation or read
			switch req.Operation {
			case "list", "get", "search":
				return c.HandleRead(ctx, req.Operation, req.Input)
			case "create", "update", "delete":
				return c.HandleMutation(ctx, req.Operation, req.Input)
			default:
				return nil, fmt.Errorf("unknown operation: %s", req.Operation)
			}
		},
	}
}

// createRoomsProvider creates a Provider for RoomsCapability
func createRoomsProvider(c *RoomsCapability) *Provider {
	return &Provider{
		Descriptor: &capv1.CapabilityDescriptor{
			CapabilityId: "product.rooms.v1",
			DisplayName:  "Rooms Management",
			Description:  "List and search Matrix rooms",
			Readiness:    true,
			Operations: []*capv1.OperationDescriptor{
				{OperationId: "list", DisplayName: "List Rooms", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "search", DisplayName: "Search Rooms", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
			},
		},
		Handler: func(ctx context.Context, input []byte) ([]byte, error) {
			var req struct {
				Operation string          `json:"operation"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &req); err != nil {
				return nil, err
			}
			return c.HandleRead(ctx, req.Operation, req.Input)
		},
	}
}

// createMessagesProvider creates a Provider for MessagesCapability
func createMessagesProvider(c *MessagesCapability) *Provider {
	return &Provider{
		Descriptor: &capv1.CapabilityDescriptor{
			CapabilityId: "product.messages.v1",
			DisplayName:  "Messages Management",
			Description:  "List and send messages in Matrix rooms",
			Readiness:    true,
			Operations: []*capv1.OperationDescriptor{
				{OperationId: "list", DisplayName: "List Messages", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "send", DisplayName: "Send Message", OperationType: capv1.OperationType_OPERATION_TYPE_MUTATION},
			},
		},
		Handler: func(ctx context.Context, input []byte) ([]byte, error) {
			var req struct {
				Operation string          `json:"operation"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &req); err != nil {
				return nil, err
			}

			switch req.Operation {
			case "list":
				return c.HandleRead(ctx, req.Operation, req.Input)
			case "send":
				return c.HandleMutation(ctx, req.Operation, req.Input)
			default:
				return nil, fmt.Errorf("unknown operation: %s", req.Operation)
			}
		},
	}
}

// createMembersProvider creates a Provider for MembersCapability
func createMembersProvider(c *MembersCapability) *Provider {
	return &Provider{
		Descriptor: &capv1.CapabilityDescriptor{
			CapabilityId: "product.members.v1",
			DisplayName:  "Room Members Management",
			Description:  "Manage room members with list, get, add, and remove operations",
			Readiness:    true,
			Operations: []*capv1.OperationDescriptor{
				{OperationId: "list", DisplayName: "List Members", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "get", DisplayName: "Get Member", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "add", DisplayName: "Add Member", OperationType: capv1.OperationType_OPERATION_TYPE_MUTATION},
				{OperationId: "remove", DisplayName: "Remove Member", OperationType: capv1.OperationType_OPERATION_TYPE_MUTATION},
			},
		},
		Handler: func(ctx context.Context, input []byte) ([]byte, error) {
			var req struct {
				Operation string          `json:"operation"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &req); err != nil {
				return nil, err
			}

			switch req.Operation {
			case "list", "get":
				return c.HandleRead(ctx, req.Operation, req.Input)
			case "add", "remove":
				return c.HandleMutation(ctx, req.Operation, req.Input)
			default:
				return nil, fmt.Errorf("unknown operation: %s", req.Operation)
			}
		},
	}
}

// createChannelsProvider creates a Provider for ChannelsCapability
func createChannelsProvider(c *ChannelsCapability) *Provider {
	return &Provider{
		Descriptor: &capv1.CapabilityDescriptor{
			CapabilityId: "product.channels.v1",
			DisplayName:  "Channels Management",
			Description:  "List channels and get posts from channels",
			Readiness:    true,
			Operations: []*capv1.OperationDescriptor{
				{OperationId: "list", DisplayName: "List Channels", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
				{OperationId: "get_posts", DisplayName: "Get Posts", OperationType: capv1.OperationType_OPERATION_TYPE_READ},
			},
		},
		Handler: func(ctx context.Context, input []byte) ([]byte, error) {
			var req struct {
				Operation string          `json:"operation"`
				Input     json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(input, &req); err != nil {
				return nil, err
			}
			return c.HandleRead(ctx, req.Operation, req.Input)
		},
	}
}
