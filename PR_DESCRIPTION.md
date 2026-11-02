# daemon/config: Phase 1 - Modular Architecture Refactoring

## Summary

This PR implements Phase 1 of the daemon/config architecture refactoring proposed in issue #53648. It introduces a modular directory structure and extracts the extensions configuration into its own sub-package without changing any underlying logic.

## Motivation

The current daemon/config architecture has several issues:

1. **Fragmented Validation**: Validation logic is scattered across the codebase
2. **Monolithic Growth**: `config.go` is becoming increasingly difficult to maintain
3. **Poor Extensibility**: Adding new components inflates the package with numerous platform-specific files

This refactoring aims to:
- Establish clear separation of concerns
- Make the codebase more modular and maintainable
- Set a predictable pattern for future component extractions

## Changes

### New Modular Structure

```
daemon/config/
├── extensions/
│   └── extensions.go       # Extension configuration (new)
├── network/                 # Prepared for future extraction
├── config.go                # Updated imports
├── extensions.go            # Backward compatibility layer
└── ...
```

### Key Changes

1. **Created `daemon/config/extensions/extensions.go`**
   - Moved `ExtensionsConfig` → `extensions.Config`
   - Moved `ExtensionConfig` → `extensions.ExtensionConfig`
   - Moved `ExtensionConfigs` → `extensions.ExtensionConfigs`
   - Preserved all validation logic and JSON marshaling

2. **Updated `daemon/config/config.go`**
   - Added import: `github.com/moby/moby/v2/daemon/config/extensions`
   - Changed field type: `Extensions extensions.Config`

3. **Maintained Backward Compatibility**
   - Added type aliases in `daemon/config/extensions.go`
   - Deprecated old type names with clear migration path
   - Existing code continues to work without modifications

## Testing

- ✅ Builds successfully on Linux (`GOOS=linux`)
- ✅ Builds successfully on Windows (`GOOS=windows`)
- ✅ No breaking changes to existing API
- ✅ All imports resolve correctly

```bash
# Verify builds
cd daemon/config
GOOS=linux go build ./...
GOOS=windows go build ./...
```

## Benefits

### Immediate
- Better code organization
- Clearer component boundaries  
- Easier navigation and maintenance

### Long-term
- Scalable pattern for extracting other components
- Foundation for implementing configuration lifecycle pipeline
- Simplified testing of individual components

## Backward Compatibility

This PR maintains 100% backward compatibility:
- Type aliases ensure existing code compiles without changes
- All public APIs remain unchanged
- Deprecation notices guide future migrations

## Next Steps (Future PRs)

**Phase 2**: Extract additional components
- Network configuration → `daemon/config/network/`
- Builder configuration → `daemon/config/builder/` (if beneficial)

**Phase 3**: Configuration source tracking
- Introduce `ConfigType` and `Path` parameters
- Implement `RawConfig` vs `Config` distinction

**Phase 4**: Configuration lifecycle pipeline
- Implement `Load`, `Merge`, `Derive`, `Validate` pipeline
- Clean up obsolete code

## Related Issues

- Closes part of #53648
- Prepares groundwork for #52674
- Prepares groundwork for #44640
- Aligns with draft PR #43703

## Checklist

- [x] Code builds successfully on Linux and Windows
- [x] No breaking changes to existing API
- [x] Backward compatibility maintained via type aliases
- [x] Documentation updated (REFACTOR_PHASE1.md)
- [x] Commit message follows conventions
- [x] Changes are focused on structural refactoring only

## Review Notes

This PR is intentionally conservative:
- **No logic changes**: Pure structural refactoring
- **No new features**: Only code organization
- **Small scope**: Single component extraction
- **Low risk**: Backward compatible with type aliases

Future PRs will build on this foundation to implement the full proposal.

---

cc @maintainers This is the first of several PRs to address #53648. I've kept it small and focused to make review easier. Please let me know if you'd like any adjustments to the approach.
