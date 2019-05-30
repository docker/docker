package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/moby/buildkit/client/buildid"
	"github.com/moby/buildkit/frontend/gateway"
	gwapi "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

type GatewayForwarder struct {
	mu         sync.RWMutex
	updateCond *sync.Cond
	builds     map[string]gateway.LLBBridgeForwarder
}

func NewGatewayForwarder() *GatewayForwarder {
	gwf := &GatewayForwarder{
		builds: map[string]gateway.LLBBridgeForwarder{},
	}
	gwf.updateCond = sync.NewCond(gwf.mu.RLocker())
	return gwf
}

func (gwf *GatewayForwarder) Register(server *grpc.Server) {
	gwapi.RegisterLLBBridgeServer(server, gwf)
}

func (gwf *GatewayForwarder) RegisterBuild(ctx context.Context, id string, bridge gateway.LLBBridgeForwarder) error {
	gwf.mu.Lock()
	defer gwf.mu.Unlock()

	if _, ok := gwf.builds[id]; ok {
		return errors.Errorf("build ID %s exists", id)
	}

	gwf.builds[id] = bridge
	gwf.updateCond.Broadcast()

	return nil
}

func (gwf *GatewayForwarder) UnregisterBuild(ctx context.Context, id string) {
	gwf.mu.Lock()
	defer gwf.mu.Unlock()

	delete(gwf.builds, id)
	gwf.updateCond.Broadcast()
}

func (gwf *GatewayForwarder) lookupForwarder(ctx context.Context) (gateway.LLBBridgeForwarder, error) {
	bid := buildid.FromIncomingContext(ctx)
	if bid == "" {
		return nil, errors.New("no buildid found in context")
	}

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	go func() {
		<-ctx.Done()
		gwf.mu.Lock()
		gwf.updateCond.Broadcast()
		gwf.mu.Unlock()
	}()

	gwf.mu.RLock()
	defer gwf.mu.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return nil, errors.Errorf("no such job %s", bid)
		default:
		}
		fwd, ok := gwf.builds[bid]
		if !ok {
			gwf.updateCond.Wait()
			continue
		}
		return fwd, nil
	}
}

func (gwf *GatewayForwarder) ResolveImageConfig(ctx context.Context, req *gwapi.ResolveImageConfigRequest) (*gwapi.ResolveImageConfigResponse, error) {
	fwd, err := gwf.lookupForwarder(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "forwarding ResolveImageConfig")
	}

	return fwd.ResolveImageConfig(ctx, req)
}

func (gwf *GatewayForwarder) Solve(ctx context.Context, req *gwapi.SolveRequest) (*gwapi.SolveResponse, error) {
	fwd, err := gwf.lookupForwarder(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "forwarding Solve")
	}

	return fwd.Solve(ctx, req)
}

func (gwf *GatewayForwarder) ReadFile(ctx context.Context, req *gwapi.ReadFileRequest) (*gwapi.ReadFileResponse, error) {
	fwd, err := gwf.lookupForwarder(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "forwarding ReadFile")
	}
	return fwd.ReadFile(ctx, req)
}

func (gwf *GatewayForwarder) Ping(ctx context.Context, req *gwapi.PingRequest) (*gwapi.PongResponse, error) {
	fwd, err := gwf.lookupForwarder(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "forwarding Ping")
	}
	return fwd.Ping(ctx, req)
}

func (gwf *GatewayForwarder) Return(ctx context.Context, req *gwapi.ReturnRequest) (*gwapi.ReturnResponse, error) {
	fwd, err := gwf.lookupForwarder(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "forwarding Return")
	}
	res, err := fwd.Return(ctx, req)
	return res, err
}

func (gwf *GatewayForwarder) ReadDir(ctx context.Context, req *gwapi.ReadDirRequest) (*gwapi.ReadDirResponse, error) {
	fwd, err := gwf.lookupForwarder(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "forwarding ReadDir")
	}
	return fwd.ReadDir(ctx, req)
}

func (gwf *GatewayForwarder) StatFile(ctx context.Context, req *gwapi.StatFileRequest) (*gwapi.StatFileResponse, error) {
	fwd, err := gwf.lookupForwarder(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "forwarding StatFile")
	}
	return fwd.StatFile(ctx, req)
}
