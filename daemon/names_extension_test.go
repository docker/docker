package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/moby/extensions"
	containernamegeneratorv0 "github.com/moby/moby/v2/extpoints/containernamegenerator/v0"
	servicenamegeneratorv0 "github.com/moby/moby/v2/extpoints/servicenamegenerator/v0"
	"github.com/moby/moby/v2/internal/testutil/staticresolver"
	"gotest.tools/v3/assert"
)

var testSinglePoint = extensions.DefineSinglePoint[any]("org.example.single.v0")

func resolvedTestProvider(id extensions.ExtensionID, impl any, builtin bool) extensions.ResolvedProvider {
	origin := extensions.ExtensionOrigin{
		Kind:       extensions.ExtensionOriginExecutable,
		Executable: &extensions.ExecutableOrigin{Path: string(id)},
	}
	if builtin {
		origin = extensions.ExtensionOrigin{Kind: extensions.ExtensionOriginBuiltin}
	}
	return extensions.ResolvedProvider{
		Identity: extensions.ExtensionIdentity{ID: id, Origin: origin},
		Impl:     impl,
	}
}

type containerNameGeneratorFunc func(context.Context, *containernamegeneratorv0.GenerateContainerNameRequest) (*containernamegeneratorv0.GenerateContainerNameReply, error)

func (f containerNameGeneratorFunc) GenerateContainerName(ctx context.Context, req *containernamegeneratorv0.GenerateContainerNameRequest) (*containernamegeneratorv0.GenerateContainerNameReply, error) {
	return f(ctx, req)
}

func TestResolveContainerName(t *testing.T) {
	t.Parallel()

	provider := containerNameGeneratorFunc(func(_ context.Context, req *containernamegeneratorv0.GenerateContainerNameRequest) (*containernamegeneratorv0.GenerateContainerNameReply, error) {
		assert.Equal(t, req.Retry, int64(3))
		assert.Equal(t, req.ContainerID, "container-id")
		assert.Equal(t, req.Image, "image:latest")
		return &containernamegeneratorv0.GenerateContainerNameReply{Name: "container-name"}, nil
	})
	resolver := staticresolver.New(resolvedTestProvider("org.example.containernamegenerator.v1", provider, true))

	name, err := resolveContainerName(t.Context(), resolver, &containernamegeneratorv0.GenerateContainerNameRequest{
		Retry:       3,
		ContainerID: "container-id",
		Image:       "image:latest",
	})
	assert.NilError(t, err)
	assert.Equal(t, name, "container-name")
}

func TestResolveContainerNameFallsBackToBuiltin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		replacementName string
		replacementErr  error
		wantErr         string
	}{
		{name: "error", replacementErr: errors.New("replacement failed"), wantErr: "replacement failed"},
		{name: "empty name", wantErr: "provider returned an empty name"},
		{name: "invalid character", replacementName: "invalid.name", wantErr: "provider returned an invalid name"},
		{name: "too short", replacementName: "x", wantErr: "provider returned an invalid name"},
		{name: "too long", replacementName: strings.Repeat("x", 64), wantErr: "provider returned an invalid name"},
		{name: "ending with separator", replacementName: "invalid_name_", wantErr: "provider returned an invalid name"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			builtinCalls := 0
			replacement := containerNameGeneratorFunc(func(context.Context, *containernamegeneratorv0.GenerateContainerNameRequest) (*containernamegeneratorv0.GenerateContainerNameReply, error) {
				if tc.replacementErr != nil {
					return nil, tc.replacementErr
				}
				return &containernamegeneratorv0.GenerateContainerNameReply{Name: tc.replacementName}, nil
			})
			builtin := containerNameGeneratorFunc(func(ctx context.Context, _ *containernamegeneratorv0.GenerateContainerNameRequest) (*containernamegeneratorv0.GenerateContainerNameReply, error) {
				_, ok := ctx.Deadline()
				assert.Assert(t, ok, "the fallback attempt must have its own deadline")
				builtinCalls++
				return &containernamegeneratorv0.GenerateContainerNameReply{Name: "builtin-name"}, nil
			})
			resolver := staticresolver.New(
				resolvedTestProvider("org.example.replacement.v1", replacement, false),
				resolvedTestProvider("org.example.builtin.v1", builtin, true),
			)

			name, err := resolveContainerName(t.Context(), resolver, &containernamegeneratorv0.GenerateContainerNameRequest{})
			assert.ErrorContains(t, err, `containernamegenerator provider "org.example.replacement.v1"`)
			assert.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, name, "builtin-name")
			assert.Equal(t, builtinCalls, 1)
		})
	}
}

type serviceNameGeneratorFunc func(context.Context, *servicenamegeneratorv0.GenerateServiceNameRequest) (*servicenamegeneratorv0.GenerateServiceNameReply, error)

func (f serviceNameGeneratorFunc) GenerateServiceName(ctx context.Context, req *servicenamegeneratorv0.GenerateServiceNameRequest) (*servicenamegeneratorv0.GenerateServiceNameReply, error) {
	return f(ctx, req)
}

func TestResolveServiceName(t *testing.T) {
	t.Parallel()

	provider := serviceNameGeneratorFunc(func(_ context.Context, req *servicenamegeneratorv0.GenerateServiceNameRequest) (*servicenamegeneratorv0.GenerateServiceNameReply, error) {
		assert.Equal(t, req.Retry, int64(3))
		assert.Equal(t, req.Image, "image:latest")
		return &servicenamegeneratorv0.GenerateServiceNameReply{Name: "service-name"}, nil
	})
	resolver := staticresolver.New(resolvedTestProvider("org.example.servicenamegenerator.v1", provider, true))

	name, err := resolveServiceName(t.Context(), resolver, &servicenamegeneratorv0.GenerateServiceNameRequest{
		Retry: 3,
		Image: "image:latest",
	})
	assert.NilError(t, err)
	assert.Equal(t, name, "service-name")
}

func TestCallSingleWithFallbackUsesReplacementProvider(t *testing.T) {
	t.Parallel()

	builtinCalled := false
	resolver := staticresolver.New(
		resolvedTestProvider("org.example.replacement.v1", "replacement", false),
		resolvedTestProvider("org.example.builtin.v1", "builtin", true),
	)
	result, err := callSingleWithFallback(t.Context(), testSinglePoint, resolver, time.Second, func(ctx context.Context, impl any) (string, error) {
		deadline, ok := ctx.Deadline()
		assert.Assert(t, ok)
		assert.Assert(t, time.Until(deadline) <= time.Second)
		if impl == "builtin" {
			builtinCalled = true
			return "builtin-result", nil
		}
		return "replacement-result", nil
	})
	assert.NilError(t, err)
	assert.Equal(t, result, "replacement-result")
	assert.Assert(t, !builtinCalled)
}

func TestCallSingleWithFallbackDoesNotRetryFailingBuiltin(t *testing.T) {
	t.Parallel()

	calls := 0
	result, err := callSingleWithFallback(t.Context(), testSinglePoint, staticresolver.New(
		resolvedTestProvider("org.example.builtin.v1", "builtin", true),
	), time.Second, func(context.Context, any) (string, error) {
		calls++
		return "partial", errors.New("builtin failed")
	})
	assert.Equal(t, result, "")
	assert.ErrorContains(t, err, `single provider "org.example.builtin.v1": builtin failed`)
	assert.Equal(t, calls, 1)
}

func TestCallSingleWithFallbackJoinsReplacementAndBuiltinErrors(t *testing.T) {
	t.Parallel()

	replacementErr := errors.New("replacement failed")
	builtinErr := errors.New("builtin failed")
	result, err := callSingleWithFallback(t.Context(), testSinglePoint, staticresolver.New(
		resolvedTestProvider("org.example.replacement.v1", replacementErr, false),
		resolvedTestProvider("org.example.builtin.v1", builtinErr, true),
	), time.Second, func(_ context.Context, impl any) (string, error) {
		return "", impl.(error)
	})
	assert.Equal(t, result, "")
	assert.Assert(t, errors.Is(err, replacementErr))
	assert.Assert(t, errors.Is(err, builtinErr))
}

func TestCallSingleWithFallbackRequiresExactlyOneEffectiveProvider(t *testing.T) {
	t.Parallel()

	fn := func(context.Context, any) (string, error) { return "result", nil }
	_, err := callSingleWithFallback(t.Context(), testSinglePoint, staticresolver.New(), time.Second, fn)
	assert.ErrorContains(t, err, "has no providers")

	_, err = callSingleWithFallback(t.Context(), testSinglePoint, staticresolver.New(
		resolvedTestProvider("org.example.one.v1", "one", false),
		resolvedTestProvider("org.example.two.v1", "two", false),
	), time.Second, fn)
	assert.ErrorContains(t, err, "has multiple providers")
}

func TestCallSingleWithFallbackPropagatesContextTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := callSingleWithFallback(ctx, testSinglePoint, staticresolver.New(
		resolvedTestProvider("org.example.slow.v1", "slow", false),
	), time.Second, func(ctx context.Context, _ any) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	assert.Assert(t, errors.Is(err, context.DeadlineExceeded))
}
