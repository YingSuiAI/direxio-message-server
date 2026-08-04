package storage

import (
	"reflect"
	"sync"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/operations"
)

// MemoryStore is an in-process implementation of the P2P product-state store.
// It is intended for tests and the legacy no-database service path only. Server
// startup must continue to require the durable PostgreSQL store.
type MemoryStore struct {
	mu sync.RWMutex

	portal    *portalState
	readMarks map[string]readMarker

	conversations map[string]conversationRecord
	channels      map[string]channel
	inviteGrants  map[string]channelInviteGrant
	posts         []channelPostRecord
	comments      []channelCommentRecord
	contacts      map[string]contactRecord
	blocks        map[string]blockRecord
	groups        map[string]groupRecord
	calls         map[string]callRecord
	favorites     map[int64]favoriteRecord
	follows       map[string]followRecord
	reactions     map[string]reactionRecord
	members       map[string]memberRecord
	events        []p2pEvent
	eventSeq      map[int64]struct{}
	eventDedupe   map[string]int64
	plugins       map[string]pluginInstance
	pluginJobs    map[string]pluginJob
	pluginSecrets map[string]map[string]pluginSecret
	reports       map[string]reportRecord
	operations    map[string]operations.Record
}

// NewMemoryStore returns an empty, concurrency-safe store. It deliberately has
// no configuration hook so it cannot silently replace durable production state.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		readMarks:     make(map[string]readMarker),
		conversations: make(map[string]conversationRecord),
		channels:      make(map[string]channel),
		inviteGrants:  make(map[string]channelInviteGrant),
		contacts:      make(map[string]contactRecord),
		blocks:        make(map[string]blockRecord),
		groups:        make(map[string]groupRecord),
		calls:         make(map[string]callRecord),
		favorites:     make(map[int64]favoriteRecord),
		follows:       make(map[string]followRecord),
		reactions:     make(map[string]reactionRecord),
		members:       make(map[string]memberRecord),
		eventSeq:      make(map[int64]struct{}),
		eventDedupe:   make(map[string]int64),
		plugins:       make(map[string]pluginInstance),
		pluginJobs:    make(map[string]pluginJob),
		pluginSecrets: make(map[string]map[string]pluginSecret),
		reports:       make(map[string]reportRecord),
		operations:    make(map[string]operations.Record),
	}
}

func cloneStringSlice(values []string) []string {
	if values == nil {
		return nil
	}
	return append([]string(nil), values...)
}

func cloneAny(value any) any {
	if value == nil {
		return nil
	}
	return cloneMemoryValue(reflect.ValueOf(value)).Interface()
}

func cloneMemoryValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneMemoryValue(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(cloneMemoryValue(iterator.Key()), cloneMemoryValue(iterator.Value()))
		}
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(cloneMemoryValue(value.Elem()))
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneMemoryValue(value.Index(i)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for i := 0; i < value.Len(); i++ {
			result.Index(i).Set(cloneMemoryValue(value.Index(i)))
		}
		return result
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		result.Set(value)
		for i := 0; i < value.NumField(); i++ {
			if result.Field(i).CanSet() && value.Type().Field(i).IsExported() {
				result.Field(i).Set(cloneMemoryValue(value.Field(i)))
			}
		}
		return result
	default:
		return value
	}
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = cloneAny(value)
	}
	return result
}

func clonePortalState(state portalState) portalState {
	state.AgentConfig.MCPBlockedRoomIDs = cloneStringSlice(state.AgentConfig.MCPBlockedRoomIDs)
	state.AgentConfig.Native = cloneAnyMap(state.AgentConfig.Native)
	return state
}

func clonePlugin(plugin pluginInstance) pluginInstance {
	plugin.Config = cloneAnyMap(plugin.Config)
	return plugin
}

func cloneEvent(event p2pEvent) p2pEvent {
	event.Payload = cloneAnyMap(event.Payload)
	return event
}

func cloneReport(report reportRecord) reportRecord {
	report.ImageURLs = cloneStringSlice(report.ImageURLs)
	return report
}
