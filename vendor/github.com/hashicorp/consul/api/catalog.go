package api

type Weights struct {
	Passing int
	Warning int
}

type Node struct {
	ID              string
	Node            string
	Address         string
	Datacenter      string
	TaggedAddresses map[string]string
	Meta            map[string]string
	CreateIndex     uint64
	ModifyIndex     uint64
}

type CatalogService struct {
	ID                       string
	Node                     string
	Address                  string
	Datacenter               string
	TaggedAddresses          map[string]string
	NodeMeta                 map[string]string
	ServiceID                string
	ServiceName              string
	ServiceAddress           string
	ServiceTags              []string
	ServiceMeta              map[string]string
	ServicePort              int
	ServiceWeights           Weights
	ServiceEnableTagOverride bool
	// DEPRECATED (ProxyDestination) - remove the next comment!
	// We forgot to ever add ServiceProxyDestination here so no need to deprecate!
	ServiceProxy *AgentServiceConnectProxyConfig
	CreateIndex  uint64
	Checks       HealthChecks
	ModifyIndex  uint64
}

type CatalogNode struct {
	Node     *Node
	Services map[string]*AgentService
}

type CatalogRegistration struct {
	ID              string
	Node            string
	Address         string
	TaggedAddresses map[string]string
	NodeMeta        map[string]string
	Datacenter      string
	Service         *AgentService
	Check           *AgentCheck
	Checks          HealthChecks
	SkipNodeUpdate  bool
}

type CatalogDeregistration struct {
	Node       string
	Address    string // Obsolete.
	Datacenter string
	ServiceID  string
	CheckID    string
}

// Catalog can be used to query the Catalog endpoints
type Catalog struct {
	c *Client
}

// Catalog returns a handle to the catalog endpoints
func (c *Client) Catalog() *Catalog {
	return &Catalog{c}
}

func (c *Catalog) Register(reg *CatalogRegistration, q *WriteOptions) (*WriteMeta, error) {
	r := c.c.newRequest("PUT", "/v1/catalog/register")
	r.setWriteOptions(q)
	r.obj = reg
	rtt, resp, err := requireOK(c.c.doRequest(r))
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	wm := &WriteMeta{}
	wm.RequestTime = rtt

	return wm, nil
}

func (c *Catalog) Deregister(dereg *CatalogDeregistration, q *WriteOptions) (*WriteMeta, error) {
	r := c.c.newRequest("PUT", "/v1/catalog/deregister")
	r.setWriteOptions(q)
	r.obj = dereg
	rtt, resp, err := requireOK(c.c.doRequest(r))
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	wm := &WriteMeta{}
	wm.RequestTime = rtt

	return wm, nil
}

// Datacenters is used to query for all the known datacenters
func (c *Catalog) Datacenters() ([]string, error) {
	r := c.c.newRequest("GET", "/v1/catalog/datacenters")
	_, resp, err := requireOK(c.c.doRequest(r))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out []string
	if err := decodeBody(resp, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Nodes is used to query all the known nodes
func (c *Catalog) Nodes(q *QueryOptions) ([]*Node, *QueryMeta, error) {
	r := c.c.newRequest("GET", "/v1/catalog/nodes")
	r.setQueryOptions(q)
	rtt, resp, err := requireOK(c.c.doRequest(r))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	qm := &QueryMeta{}
	parseQueryMeta(resp, qm)
	qm.RequestTime = rtt

	var out []*Node
	if err := decodeBody(resp, &out); err != nil {
		return nil, nil, err
	}
	return out, qm, nil
}

// Services is used to query for all known services
func (c *Catalog) Services(q *QueryOptions) (map[string][]string, *QueryMeta, error) {
	r := c.c.newRequest("GET", "/v1/catalog/services")
	r.setQueryOptions(q)
	rtt, resp, err := requireOK(c.c.doRequest(r))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	qm := &QueryMeta{}
	parseQueryMeta(resp, qm)
	qm.RequestTime = rtt

	var out map[string][]string
	if err := decodeBody(resp, &out); err != nil {
		return nil, nil, err
	}
	return out, qm, nil
}

// Service is used to query catalog entries for a given service
func (c *Catalog) Service(service, tag string, q *QueryOptions) ([]*CatalogService, *QueryMeta, error) {
	var tags []string
	if tag != "" {
		tags = []string{tag}
	}
	return c.service(service, tags, q, false)
}

// Supports multiple tags for filtering
func (c *Catalog) ServiceMultipleTags(service string, tags []string, q *QueryOptions) ([]*CatalogService, *QueryMeta, error) {
	return c.service(service, tags, q, false)
}

// Connect is used to query catalog entries for a given Connect-enabled service
func (c *Catalog) Connect(service, tag string, q *QueryOptions) ([]*CatalogService, *QueryMeta, error) {
	var tags []string
	if tag != "" {
		tags = []string{tag}
	}
	return c.service(service, tags, q, true)
}

// Supports multiple tags for filtering
func (c *Catalog) ConnectMultipleTags(service string, tags []string, q *QueryOptions) ([]*CatalogService, *QueryMeta, error) {
	return c.service(service, tags, q, true)
}

func (c *Catalog) service(service string, tags []string, q *QueryOptions, connect bool) ([]*CatalogService, *QueryMeta, error) {
	path := "/v1/catalog/service/" + service
	if connect {
		path = "/v1/catalog/connect/" + service
	}
	r := c.c.newRequest("GET", path)
	r.setQueryOptions(q)
	if len(tags) > 0 {
		for _, tag := range tags {
			r.params.Add("tag", tag)
		}
	}
	rtt, resp, err := requireOK(c.c.doRequest(r))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	qm := &QueryMeta{}
	parseQueryMeta(resp, qm)
	qm.RequestTime = rtt

	var out []*CatalogService
	if err := decodeBody(resp, &out); err != nil {
		return nil, nil, err
	}
	return out, qm, nil
}

// Node is used to query for service information about a single node
func (c *Catalog) Node(node string, q *QueryOptions) (*CatalogNode, *QueryMeta, error) {
	r := c.c.newRequest("GET", "/v1/catalog/node/"+node)
	r.setQueryOptions(q)
	rtt, resp, err := requireOK(c.c.doRequest(r))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	qm := &QueryMeta{}
	parseQueryMeta(resp, qm)
	qm.RequestTime = rtt

	var out *CatalogNode
	if err := decodeBody(resp, &out); err != nil {
		return nil, nil, err
	}
	return out, qm, nil
}
