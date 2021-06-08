package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mitchellh/mapstructure"
)

const (
	ServiceDefaults   string = "service-defaults"
	ProxyDefaults     string = "proxy-defaults"
	ProxyConfigGlobal string = "global"
)

type ConfigEntry interface {
	GetKind() string
	GetName() string
	GetCreateIndex() uint64
	GetModifyIndex() uint64
}

type ServiceConfigEntry struct {
	Kind        string
	Name        string
	Protocol    string
	CreateIndex uint64
	ModifyIndex uint64
}

func (s *ServiceConfigEntry) GetKind() string {
	return s.Kind
}

func (s *ServiceConfigEntry) GetName() string {
	return s.Name
}

func (s *ServiceConfigEntry) GetCreateIndex() uint64 {
	return s.CreateIndex
}

func (s *ServiceConfigEntry) GetModifyIndex() uint64 {
	return s.ModifyIndex
}

type ProxyConfigEntry struct {
	Kind        string
	Name        string
	Config      map[string]interface{}
	CreateIndex uint64
	ModifyIndex uint64
}

func (p *ProxyConfigEntry) GetKind() string {
	return p.Kind
}

func (p *ProxyConfigEntry) GetName() string {
	return p.Name
}

func (p *ProxyConfigEntry) GetCreateIndex() uint64 {
	return p.CreateIndex
}

func (p *ProxyConfigEntry) GetModifyIndex() uint64 {
	return p.ModifyIndex
}

type rawEntryListResponse struct {
	kind    string
	Entries []map[string]interface{}
}

func makeConfigEntry(kind, name string) (ConfigEntry, error) {
	switch kind {
	case ServiceDefaults:
		return &ServiceConfigEntry{Name: name}, nil
	case ProxyDefaults:
		return &ProxyConfigEntry{Name: name}, nil
	default:
		return nil, fmt.Errorf("invalid config entry kind: %s", kind)
	}
}

func DecodeConfigEntry(raw map[string]interface{}) (ConfigEntry, error) {
	var entry ConfigEntry

	kindVal, ok := raw["Kind"]
	if !ok {
		kindVal, ok = raw["kind"]
	}
	if !ok {
		return nil, fmt.Errorf("Payload does not contain a kind/Kind key at the top level")
	}

	if kindStr, ok := kindVal.(string); ok {
		newEntry, err := makeConfigEntry(kindStr, "")
		if err != nil {
			return nil, err
		}
		entry = newEntry
	} else {
		return nil, fmt.Errorf("Kind value in payload is not a string")
	}

	decodeConf := &mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		Result:           &entry,
		WeaklyTypedInput: true,
	}

	decoder, err := mapstructure.NewDecoder(decodeConf)
	if err != nil {
		return nil, err
	}

	return entry, decoder.Decode(raw)
}

func DecodeConfigEntryFromJSON(data []byte) (ConfigEntry, error) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	return DecodeConfigEntry(raw)
}

// Config can be used to query the Config endpoints
type ConfigEntries struct {
	c *Client
}

// Config returns a handle to the Config endpoints
func (c *Client) ConfigEntries() *ConfigEntries {
	return &ConfigEntries{c}
}

func (conf *ConfigEntries) Get(kind string, name string, q *QueryOptions) (ConfigEntry, *QueryMeta, error) {
	if kind == "" || name == "" {
		return nil, nil, fmt.Errorf("Both kind and name parameters must not be empty")
	}

	entry, err := makeConfigEntry(kind, name)
	if err != nil {
		return nil, nil, err
	}

	r := conf.c.newRequest("GET", fmt.Sprintf("/v1/config/%s/%s", kind, name))
	r.setQueryOptions(q)
	rtt, resp, err := requireOK(conf.c.doRequest(r))
	if err != nil {
		return nil, nil, err
	}

	defer resp.Body.Close()

	qm := &QueryMeta{}
	parseQueryMeta(resp, qm)
	qm.RequestTime = rtt

	if err := decodeBody(resp, entry); err != nil {
		return nil, nil, err
	}

	return entry, qm, nil
}

func (conf *ConfigEntries) List(kind string, q *QueryOptions) ([]ConfigEntry, *QueryMeta, error) {
	if kind == "" {
		return nil, nil, fmt.Errorf("The kind parameter must not be empty")
	}

	r := conf.c.newRequest("GET", fmt.Sprintf("/v1/config/%s", kind))
	r.setQueryOptions(q)
	rtt, resp, err := requireOK(conf.c.doRequest(r))
	if err != nil {
		return nil, nil, err
	}

	defer resp.Body.Close()

	qm := &QueryMeta{}
	parseQueryMeta(resp, qm)
	qm.RequestTime = rtt

	var raw []map[string]interface{}
	if err := decodeBody(resp, &raw); err != nil {
		return nil, nil, err
	}

	var entries []ConfigEntry
	for _, rawEntry := range raw {
		entry, err := DecodeConfigEntry(rawEntry)
		if err != nil {
			return nil, nil, err
		}
		entries = append(entries, entry)
	}

	return entries, qm, nil
}

func (conf *ConfigEntries) Set(entry ConfigEntry, w *WriteOptions) (bool, *WriteMeta, error) {
	return conf.set(entry, nil, w)
}

func (conf *ConfigEntries) CAS(entry ConfigEntry, index uint64, w *WriteOptions) (bool, *WriteMeta, error) {
	return conf.set(entry, map[string]string{"cas": strconv.FormatUint(index, 10)}, w)
}

func (conf *ConfigEntries) set(entry ConfigEntry, params map[string]string, w *WriteOptions) (bool, *WriteMeta, error) {
	r := conf.c.newRequest("PUT", "/v1/config")
	r.setWriteOptions(w)
	for param, value := range params {
		r.params.Set(param, value)
	}
	r.obj = entry
	rtt, resp, err := requireOK(conf.c.doRequest(r))
	if err != nil {
		return false, nil, err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return false, nil, fmt.Errorf("Failed to read response: %v", err)
	}
	res := strings.Contains(buf.String(), "true")

	wm := &WriteMeta{RequestTime: rtt}
	return res, wm, nil
}

func (conf *ConfigEntries) Delete(kind string, name string, w *WriteOptions) (*WriteMeta, error) {
	if kind == "" || name == "" {
		return nil, fmt.Errorf("Both kind and name parameters must not be empty")
	}

	r := conf.c.newRequest("DELETE", fmt.Sprintf("/v1/config/%s/%s", kind, name))
	r.setWriteOptions(w)
	rtt, resp, err := requireOK(conf.c.doRequest(r))
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	wm := &WriteMeta{RequestTime: rtt}
	return wm, nil
}
