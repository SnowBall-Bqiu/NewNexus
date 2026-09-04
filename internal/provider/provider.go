package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

var ErrUnsupported = errors.New("provider capability is not supported")

type Credentials map[string]string

type Capability struct {
	DNS          bool              `json:"dns"`
	Lines        bool              `json:"lines"`
	Weights      bool              `json:"weights"`
	ApexCNAME    bool              `json:"apexCname"`
	SaaS         bool              `json:"saas"`
	BatchChanges bool              `json:"batchChanges"`
	Details      map[string]string `json:"details,omitempty"`
}

type Zone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

type Line struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type Record struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	LineCode string `json:"lineCode"`
	TTL      int    `json:"ttl"`
	Weight   int    `json:"weight"`
	Enabled  bool   `json:"enabled"`
	Proxied  bool   `json:"proxied"`
	Comment  string `json:"comment"`
	Managed  bool   `json:"managed"`
}

type RecordChange struct {
	Operation string  `json:"operation"`
	Before    *Record `json:"before,omitempty"`
	After     *Record `json:"after,omitempty"`
}

type Adapter interface {
	Name() string
	Validate(context.Context, Credentials) (Capability, error)
	ListZones(context.Context, Credentials) ([]Zone, error)
	ListLines(context.Context, Credentials, Zone) ([]Line, error)
	ListRecords(context.Context, Credentials, Zone, string) ([]Record, error)
	ApplyChanges(context.Context, Credentials, Zone, []RecordChange) ([]Record, error)
}

type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: make(map[string]Adapter)}
	for _, adapter := range adapters {
		r.adapters[adapter.Name()] = adapter
	}
	return r
}

func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[strings.ToLower(name)]
	return adapter, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	return names
}

func ParseCredentials(raw string) (Credentials, error) {
	credentials := Credentials{}
	if err := json.Unmarshal([]byte(raw), &credentials); err != nil {
		return nil, fmt.Errorf("decode credentials: %w", err)
	}
	return credentials, nil
}

func Required(credentials Credentials, fields ...string) error {
	missing := make([]string, 0)
	for _, field := range fields {
		if strings.TrimSpace(credentials[field]) == "" {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing credential fields: %s", strings.Join(missing, ", "))
	}
	return nil
}
