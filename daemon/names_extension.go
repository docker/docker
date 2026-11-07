package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/moby/extensions"
	containernamegeneratorv0 "github.com/moby/moby/v2/extpoints/containernamegenerator/v0"
	servicenamegeneratorv0 "github.com/moby/moby/v2/extpoints/servicenamegenerator/v0"
)

const nameProviderTimeout = 5 * time.Second

// resolveContainerName calls the effective container name provider.
func resolveContainerName(ctx context.Context, resolver extensions.Resolver, req *containernamegeneratorv0.GenerateContainerNameRequest) (string, error) {
	return callSingleWithFallback(ctx, containernamegeneratorv0.Point, resolver, nameProviderTimeout, func(ctx context.Context, generator containernamegeneratorv0.ContainerNameGenerator) (string, error) {
		reply, err := generator.GenerateContainerName(ctx, req)
		if err != nil {
			return "", err
		}
		if reply == nil {
			return "", errors.New("provider returned a nil reply")
		}
		return reply.Name, validateGeneratedName(reply.Name)
	})
}

// resolveServiceName calls the effective service name provider.
func resolveServiceName(ctx context.Context, resolver extensions.Resolver, req *servicenamegeneratorv0.GenerateServiceNameRequest) (string, error) {
	return callSingleWithFallback(ctx, servicenamegeneratorv0.Point, resolver, nameProviderTimeout, func(ctx context.Context, generator servicenamegeneratorv0.ServiceNameGenerator) (string, error) {
		reply, err := generator.GenerateServiceName(ctx, req)
		if err != nil {
			return "", err
		}
		if reply == nil {
			return "", errors.New("provider returned a nil reply")
		}
		return reply.Name, validateGeneratedName(reply.Name)
	})
}

func validateGeneratedName(name string) error {
	if name == "" {
		return errors.New("provider returned an empty name")
	}
	if !isValidGeneratedName(name) {
		return errors.New("provider returned an invalid name: expected 2-63 ASCII letters, digits, hyphens, or underscores, starting and ending with a letter or digit")
	}
	return nil
}

func isValidGeneratedName(name string) bool {
	if len(name) < 2 || len(name) > 63 {
		return false
	}
	for i := range len(name) {
		ch := name[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		if i > 0 && i < len(name)-1 && (ch == '-' || ch == '_') {
			continue
		}
		return false
	}
	return true
}

// callSingleWithFallback calls the effective provider of a single point.
// An executable provider hides the built-in during normal resolution, but the
// full provider list retains it as a fallback.
// If the executable provider fails and the built-in succeeds, it returns the
// built-in result together with the executable provider's error so callers can
// report the extension failure without blocking the operation.
// If the built-in also fails, the returned error contains both failures.
// Each provider call gets its own timeout.
//
// It is the single-point counterpart of [extensions.Each] and
// [extensions.Fold]; move it to github.com/moby/extensions once points outside
// name generation need it.
func callSingleWithFallback[T, R any](ctx context.Context, point extensions.Point[T], resolver extensions.Resolver, timeout time.Duration, fn func(context.Context, T) (R, error)) (R, error) {
	providers := resolver.Providers(point.ID())
	primary, err := effectiveSingleProvider(point.ID(), providers)
	if err != nil {
		var zero R
		return zero, err
	}

	result, primaryErr := callTypedProvider(ctx, point, primary, timeout, fn)
	if primaryErr == nil || primary.Identity.Origin.Kind == extensions.ExtensionOriginBuiltin {
		return result, primaryErr
	}

	fallback, ok := findBuiltinFallback(providers, primary)
	if !ok {
		return result, primaryErr
	}

	result, fallbackErr := callTypedProvider(ctx, point, fallback, timeout, fn)
	return result, errors.Join(primaryErr, fallbackErr)
}

func effectiveSingleProvider(point extensions.PointID, providers []extensions.ResolvedProvider) (extensions.ResolvedProvider, error) {
	effective := extensions.EffectiveProviders(providers)
	switch len(effective) {
	case 0:
		return extensions.ResolvedProvider{}, fmt.Errorf("point %q has no providers", point)
	case 1:
		return effective[0], nil
	default:
		return extensions.ResolvedProvider{}, fmt.Errorf("point %q has multiple providers", point)
	}
}

func findBuiltinFallback(providers []extensions.ResolvedProvider, primary extensions.ResolvedProvider) (extensions.ResolvedProvider, bool) {
	for _, provider := range providers {
		if provider.Identity.Origin.Kind == extensions.ExtensionOriginBuiltin && provider.Identity.ID != primary.Identity.ID {
			return provider, true
		}
	}
	return extensions.ResolvedProvider{}, false
}

func callTypedProvider[T, R any](ctx context.Context, point extensions.Point[T], provider extensions.ResolvedProvider, timeout time.Duration, fn func(context.Context, T) (R, error)) (R, error) {
	var zero R
	impl, ok := provider.Impl.(T)
	if !ok {
		return zero, providerError(point.ID(), provider.Identity.ID, fmt.Errorf("provider has type %T", provider.Impl))
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := fn(ctx, impl)
	if err != nil {
		return zero, providerError(point.ID(), provider.Identity.ID, err)
	}
	return result, nil
}

func providerError(point extensions.PointID, extension extensions.ExtensionID, err error) error {
	return fmt.Errorf("%s provider %q: %w", point.Name(), extension, err)
}
