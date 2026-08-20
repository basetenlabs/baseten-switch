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
	inspectionRoot, err := os.MkdirTemp("", ".baseten-switch-decode-inspect-*")
	if err != nil {
		return plan, fmt.Errorf("trace package decode: create inspection staging: %w", err)
	}
	if err := os.Chmod(inspectionRoot, 0o700); err != nil {
		_ = os.RemoveAll(inspectionRoot)
		return plan, err
	}
	inspectionRoot, err = filepath.EvalSymlinks(inspectionRoot)
	if err != nil {
		return plan, fmt.Errorf("trace package decode: resolve inspection staging: %w", err)
	}
	inspectionInfo, err := os.Lstat(inspectionRoot)
	if err != nil {
		return plan, err
	}
	inspectionIdentity, err := identityFromFileInfo(inspectionInfo)
	if err != nil {
		return plan, err
	}
	defer func() {
		returnErr = errors.Join(returnErr, removeIdentityVerifiedDirectory(inspectionRoot, inspectionIdentity))
	}()
	inspectionOutput := filepath.Join(inspectionRoot, "not-published")
	executed, err := decodeLegacy(ctx, DecodeOptions{PackagePath: resolved, OutputDir: inspectionOutput, inspectOnly: true})
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
	file, identity, resolved, err := openSecurePackage(plan.state.packagePath)
	if err != nil {
		return result, err
	}
	closeErr := file.Close()
	if closeErr != nil || resolved != plan.state.packagePath || identity != plan.state.packageInfo {
		return result, errors.New("trace package decode: source package changed after inspection")
	}
	executed, err := decodeLegacy(ctx, DecodeOptions{PackagePath: resolved, OutputDir: outputDir})
	if err != nil {
		return result, err
	}
	if executed.PackageSHA256 != plan.Preflight.PackageSHA256 || executed.ArchiveID != plan.Preflight.ArchiveID ||
		executed.TraceCount != plan.Preflight.TraceCount || executed.BodyCount != plan.Preflight.CapturedBodyCount || executed.DecodedBytes != plan.Preflight.DecodedBytes {
		if outputInfo, statErr := os.Lstat(executed.OutputDir); statErr == nil {
			if outputIdentity, identityErr := identityFromFileInfo(outputInfo); identityErr == nil {
				_ = removeIdentityVerifiedDirectory(executed.OutputDir, outputIdentity)
			}
		}
		return result, errors.New("trace package decode: repeated validation differs from inspected plan")
	}
	return DecodeResult{
		OutputDir: executed.OutputDir, ArchiveID: executed.ArchiveID,
		PackageSHA256: executed.PackageSHA256, TraceCount: executed.TraceCount,
		MaterializedBodies: executed.BodyCount, DecodedBytes: executed.DecodedBytes,
	}, nil
}

func removeIdentityVerifiedDirectory(path string, identity fileIdentity) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !sameObjectIdentity(info, identity) {
		return errors.New("trace package decode: refused cleanup after directory identity changed")
	}
	return os.RemoveAll(path)
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
