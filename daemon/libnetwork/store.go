package libnetwork

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/datastore"
	"go.opentelemetry.io/otel"
)

func (c *Controller) getNetworkFromStore(nid string) (*Network, error) {
	for _, n := range c.getNetworksFromStore(context.TODO()) {
		if n.id == nid {
			return n, nil
		}
	}
	return nil, ErrNoSuchNetwork(nid)
}

func (c *Controller) getNetworks() ([]*Network, error) {
	var nl []*Network

	kvol, err := c.store.List(&Network{ctrlr: c})
	if err != nil && !errors.Is(err, datastore.ErrKeyNotFound) {
		return nil, fmt.Errorf("failed to get networks: %w", err)
	}

	for _, kvo := range kvol {
		n := kvo.(*Network)
		c.cacheNetwork(n)
		nl = append(nl, n)
	}

	return nl, nil
}

func (c *Controller) getNetworksFromStore(ctx context.Context) []*Network { // FIXME: unify with c.getNetworks()
	var nl []*Network

	kvol, err := c.store.List(&Network{ctrlr: c})
	if err != nil {
		if !errors.Is(err, datastore.ErrKeyNotFound) {
			log.G(ctx).Debugf("failed to get networks from store: %v", err)
		}
		return nil
	}

	for _, kvo := range kvol {
		n := kvo.(*Network)
		nl = append(nl, n)
	}

	return nl
}

func (n *Network) getEndpointFromStore(eid string) (*Endpoint, error) {
	n.ctrlr.endpointStoreMu.RLock()
	defer n.ctrlr.endpointStoreMu.RUnlock()
	ep := &Endpoint{id: eid, network: n}
	err := n.ctrlr.store.GetObject(ep)
	if err != nil {
		return nil, fmt.Errorf("could not find endpoint %s: %w", eid, err)
	}
	n.ctrlr.cacheEndpoint(ep)
	return ep, nil
}

func (n *Network) getEndpointsFromStore() ([]*Endpoint, error) {
	var epl []*Endpoint

	n.ctrlr.endpointStoreMu.RLock()
	defer n.ctrlr.endpointStoreMu.RUnlock()
	kvol, err := n.getController().store.List(&Endpoint{network: n})
	if err != nil {
		if !errors.Is(err, datastore.ErrKeyNotFound) {
			return nil, fmt.Errorf("failed to get endpoints for network %s: %w",
				n.Name(), err)
		}
		return nil, nil
	}

	for _, kvo := range kvol {
		ep := kvo.(*Endpoint)
		epl = append(epl, ep)
		n.ctrlr.cacheEndpoint(ep)
	}

	return epl, nil
}

func (c *Controller) updateToStore(ctx context.Context, kvObject datastore.KVObject) error {
	ctx, span := otel.Tracer("").Start(ctx, "libnetwork.Controller.updateToStore")
	defer span.End()

	if err := c.store.PutObjectAtomic(kvObject); err != nil {
		if errors.Is(err, datastore.ErrKeyModified) {
			return err
		}
		return fmt.Errorf("failed to update store for object type %T: %v", kvObject, err)
	}

	return nil
}

func (c *Controller) deleteFromStore(kvObject datastore.KVObject) error {
retry:
	if err := c.store.DeleteObjectAtomic(kvObject); err != nil {
		if errors.Is(err, datastore.ErrKeyModified) {
			if err := c.store.GetObject(kvObject); err != nil {
				return fmt.Errorf("could not update the kvobject to latest when trying to delete: %v", err)
			}
			log.G(context.TODO()).Warnf("Error (%v) deleting object %v, retrying....", err, kvObject.Key())
			goto retry
		}
		return err
	}

	return nil
}

func (c *Controller) networkCleanup() {
	for _, n := range c.getNetworksFromStore(context.TODO()) {
		if n.inDelete {
			log.G(context.TODO()).Infof("Removing stale network %s (%s)", n.Name(), n.ID())
			if err := n.delete(true, true); err != nil {
				log.G(context.TODO()).Debugf("Error while removing stale network: %v", err)
			}
		}
	}
}
