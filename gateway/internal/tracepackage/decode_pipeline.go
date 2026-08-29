package tracepackage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// InspectDecode performs the authoritative validation and bounded body
// materialization pass without creating the requested output directory.
func InspectDecode(ctx context.Context, packagePath string) (plan DecodePlan, returnErr error) {
	file, identity, resolved, err := openSecurePackage(packagePath)
	if err != nil {
		return plan, err
	}
	if err := file.Close(); err != nil {
		return plan, err
	}
	inspectionName, err := randomDecodeStageName()
	if err != nil {
		return plan, err
	}
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return plan, fmt.Errorf("trace package decode: resolve temporary directory: %w", err)
	}
	inspectionWorkspace, err := createDecodeWorkspace(filepath.Join(tempRoot, inspectionName+"-output"))
	if err != nil {
		return plan, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, inspectionWorkspace.cleanup())
	}()
	inspectionOutput := filepath.Join(inspectionWorkspace.stagePath, "not-published")
	executed, err := decodeLegacy(ctx, DecodeOptions{
		PackagePath: resolved, OutputDir: inspectionOutput, inspectOnly: true,
		expectedPackageIdentity: &identity,
	})
	if err != nil {
		return plan, err
	}
	verifiedFile, verifiedIdentity, verifiedPath, err := openSecurePackage(resolved)
	if err != nil {
		return plan, err
	}
	verifiedHash, hashErr := hashOpenFile(verifiedFile, verifiedIdentity.size)
	closeErr := verifiedFile.Close()
	if hashErr != nil || closeErr != nil || verifiedPath != resolved || verifiedIdentity != identity || verifiedHash != executed.PackageSHA256 {
		return plan, errors.New("trace package decode: source package changed during inspection")
	}
	plan.Preflight = DecodePreflight{
		ArchiveID: executed.ArchiveID, PackageSHA256: executed.PackageSHA256,
		TraceCount: executed.TraceCount, CapturedBodyCount: executed.BodyCount,
		OmittedBodyCount: executed.TraceCount*2 - executed.BodyCount, DecodedBytes: executed.DecodedBytes,
		MemberNames: slices.Clone(executed.MemberNames), TelemetryRows: executed.TelemetryCount,
		NativeRows: executed.NativeTurnCount, Scanner: executed.Scanner,
	}
	plan.state = &decodePlanState{packagePath: resolved, packageInfo: identity}
	return plan, nil
}

// MaterializeDecode repeats authoritative verification and writes a new
// decoded directory. A plan may be used only once.
func MaterializeDecode(ctx context.Context, plan DecodePlan, outputDir string) (DecodeResult, error) {
	var result DecodeResult
	if plan.state == nil {
		return result, errors.New("trace package decode: invalid decode plan")
	}
	plan.state.mu.Lock()
	if plan.state.used {
		plan.state.mu.Unlock()
		return result, errors.New("trace package decode: decode plan was already used")
	}
	plan.state.used = true
	plan.state.mu.Unlock()
	executed, err := decodeLegacy(ctx, DecodeOptions{
		PackagePath: plan.state.packagePath, OutputDir: outputDir,
		expectedPackageIdentity: &plan.state.packageInfo,
		expectedPreflight:       &plan.Preflight,
	})
	if err != nil {
		return result, err
	}
	return DecodeResult{
		OutputDir: executed.OutputDir, ArchiveID: executed.ArchiveID,
		PackageSHA256: executed.PackageSHA256, TraceCount: executed.TraceCount,
		MaterializedBodies: executed.BodyCount, DecodedBytes: executed.DecodedBytes,
	}, nil
}

// Decode is a convenience API for callers that do not need an interactive
// confirmation between inspection and materialization.
func Decode(ctx context.Context, options DecodeOptions) (DecodeResult, error) {
	plan, err := InspectDecode(ctx, options.PackagePath)
	if err != nil {
		return DecodeResult{}, err
	}
	return MaterializeDecode(ctx, plan, options.OutputDir)
}
