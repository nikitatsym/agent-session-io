package sessionio

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

// Adapter discovers and reads sessions for one harness.
type Adapter interface {
	Descriptor() AdapterDescriptor
	Sources(context.Context) (Stream[Source], error)
	Sessions(context.Context, SessionRequest) (Stream[SessionRef], error)
	Read(context.Context, SessionRef) (Stream[ReadItem], error)
}

// Registry stores one validated adapter per harness.
type Registry struct {
	mu      sync.RWMutex
	entries map[Harness]registryEntry
}

type registryEntry struct {
	adapter    Adapter
	descriptor AdapterDescriptor
}

// NewRegistry creates a registry and validates every supplied adapter.
func NewRegistry(adapters ...Adapter) (*Registry, error) {
	registry := &Registry{
		entries: make(map[Harness]registryEntry, len(adapters)),
	}
	for _, adapter := range adapters {
		if err := registry.Register(adapter); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// Register adds an adapter after validating its descriptor.
func (registry *Registry) Register(adapter Adapter) error {
	if registry == nil {
		return errors.New("sessionio: register adapter on nil registry")
	}
	if nilInterface(adapter) {
		return errors.New("sessionio: adapter must not be nil")
	}

	descriptor := adapter.Descriptor()
	if err := validateAdapterDescriptor(descriptor); err != nil {
		return err
	}
	descriptor = cloneDescriptor(descriptor)

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.entries[descriptor.Harness]; exists {
		return fmt.Errorf("sessionio: adapter for harness %q is already registered", descriptor.Harness)
	}
	registry.entries[descriptor.Harness] = registryEntry{
		adapter:    adapter,
		descriptor: descriptor,
	}
	return nil
}

// Adapter returns the adapter registered for a harness.
func (registry *Registry) Adapter(harness Harness) (Adapter, bool) {
	if registry == nil {
		return nil, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	entry, found := registry.entries[harness]
	return entry.adapter, found
}

// Descriptors returns descriptors ordered lexically by harness.
func (registry *Registry) Descriptors() []AdapterDescriptor {
	if registry == nil {
		return []AdapterDescriptor{}
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()

	descriptors := make([]AdapterDescriptor, 0, len(registry.entries))
	for _, entry := range registry.entries {
		descriptors = append(descriptors, cloneDescriptor(entry.descriptor))
	}
	sort.Slice(descriptors, func(left, right int) bool {
		return descriptors[left].Harness < descriptors[right].Harness
	})
	return descriptors
}

func cloneDescriptor(descriptor AdapterDescriptor) AdapterDescriptor {
	descriptor.Capabilities = append([]CapabilityStatus(nil), descriptor.Capabilities...)
	return descriptor
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
