package claudedesktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

const telemetryResourceOverride = "CLAUDE_DESKTOP_RESOURCES_PATH"

var (
	segmentMaterialPattern = regexp.MustCompile(`segmentKey\s*:\s*["']([A-Za-z0-9_-]{20,128})["']\s*,\s*segmentCdnHost\s*:\s*["']a-cdn\.anthropic\.com["']\s*,\s*segmentApiHost\s*:\s*["']a-api\.anthropic\.com["']`)
	rumMaterialPattern     = regexp.MustCompile(`applicationId\s*:\s*["']([0-9a-fA-F-]{36})["']\s*,\s*clientToken\s*:\s*["']([A-Za-z0-9_-]{20,128})["']\s*,\s*site\s*:\s*["']us5\.datadoghq\.com["']`)
	sentryMaterialPattern  = regexp.MustCompile(`https://([A-Za-z0-9_-]{20,128})@o1158394\.ingest\.us\.sentry\.io/4507368973008896`)
	datadogLogsToken       = regexp.MustCompile(`[A-Za-z0-9_-]{35}`)
)

type telemetryMaterialSources struct {
	resourceArchives []string
	webRoots         []string
	codeBinaries     []string
}

// ResolveTelemetryMaterials extracts the public ingestion configuration from
// the locally installed official Claude Desktop distribution. Values are
// returned only in memory and are subsequently persisted inside the same
// encrypted credential envelope as the Desktop OAuth enrollment.
func ResolveTelemetryMaterials(ctx context.Context, desktopVersion string) (TelemetryMaterials, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sources := discoverTelemetryMaterialSources(desktopVersion)
	return resolveTelemetryMaterialsFromSources(ctx, sources)
}

func resolveTelemetryMaterialsFromSources(ctx context.Context, sources telemetryMaterialSources) (TelemetryMaterials, error) {
	var materials TelemetryMaterials
	for _, root := range sources.webRoots {
		if errContext := ctx.Err(); errContext != nil {
			return TelemetryMaterials{}, errContext
		}
		if materials.SegmentWriteKey != "" && materials.DatadogRUMClientToken != "" && materials.DatadogRUMApplicationID != "" {
			break
		}
		paths := make([]string, 0, 128)
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry == nil {
				return nil
			}
			if errContext := ctx.Err(); errContext != nil {
				return errContext
			}
			if entry.IsDir() {
				return nil
			}
			extension := strings.ToLower(filepath.Ext(entry.Name()))
			if extension != ".js" && extension != ".html" {
				return nil
			}
			info, errInfo := entry.Info()
			if errInfo != nil || info.Size() <= 0 || info.Size() > 64<<20 {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		sort.SliceStable(paths, func(left, right int) bool {
			leftPriority := telemetryWebAssetPriority(paths[left])
			rightPriority := telemetryWebAssetPriority(paths[right])
			if leftPriority == rightPriority {
				return paths[left] < paths[right]
			}
			return leftPriority < rightPriority
		})
		for _, path := range paths {
			if errContext := ctx.Err(); errContext != nil {
				return TelemetryMaterials{}, errContext
			}
			payload, errRead := os.ReadFile(path)
			if errRead != nil {
				continue
			}
			if materials.SegmentWriteKey == "" {
				if match := segmentMaterialPattern.FindSubmatch(payload); len(match) == 2 {
					materials.SegmentWriteKey = string(match[1])
				}
			}
			if materials.DatadogRUMClientToken == "" || materials.DatadogRUMApplicationID == "" {
				if match := rumMaterialPattern.FindSubmatch(payload); len(match) == 3 {
					materials.DatadogRUMApplicationID = string(match[1])
					materials.DatadogRUMClientToken = string(match[2])
				}
			}
			if materials.SegmentWriteKey != "" && materials.DatadogRUMClientToken != "" && materials.DatadogRUMApplicationID != "" {
				break
			}
		}
	}
	for _, archive := range sources.resourceArchives {
		if materials.SentryPublicKey != "" {
			break
		}
		match, errMatch := firstFileMatch(ctx, archive, sentryMaterialPattern, 1024)
		if errMatch == nil && len(match) == 2 {
			materials.SentryPublicKey = string(match[1])
		}
	}
	const datadogLogsEndpoint = "https://http-intake.logs.us5.datadoghq.com/api/v2/logs"
	for _, binary := range sources.codeBinaries {
		if materials.DatadogLogsAPIKey != "" {
			break
		}
		window, errWindow := fileWindowAfter(ctx, binary, []byte(datadogLogsEndpoint), 512)
		if errWindow != nil {
			continue
		}
		if match := datadogLogsToken.Find(window); len(match) == 35 {
			materials.DatadogLogsAPIKey = string(match)
		}
	}
	if errValidate := materials.Validate(); errValidate != nil {
		return TelemetryMaterials{}, fmt.Errorf("resolve Claude Desktop telemetry materials: %w", errValidate)
	}
	return materials, nil
}

func discoverTelemetryMaterialSources(desktopVersion string) telemetryMaterialSources {
	var resourceRoots []string
	var codeBinaries []string
	for _, override := range filepath.SplitList(strings.TrimSpace(os.Getenv(telemetryResourceOverride))) {
		override = strings.TrimSpace(override)
		if override == "" {
			continue
		}
		if info, errStat := os.Stat(override); errStat == nil && !info.IsDir() {
			if strings.EqualFold(filepath.Base(override), "claude.exe") {
				codeBinaries = append(codeBinaries, override)
			} else {
				resourceRoots = append(resourceRoots, filepath.Dir(override))
			}
			continue
		}
		resourceRoots = append(resourceRoots, override)
		codeBinaries = append(codeBinaries, globFiles(filepath.Join(override, "claude-code", "*", "claude.exe"))...)
	}
	if runtime.GOOS == "windows" {
		version := strings.TrimSpace(desktopVersion)
		programFiles := strings.TrimSpace(os.Getenv("ProgramFiles"))
		if programFiles != "" && version != "" {
			resourceRoots = append(resourceRoots, globDirectories(filepath.Join(programFiles, "WindowsApps", "Claude_"+version+"_*__pzs8sxrjxfjjc", "app", "resources"))...)
		}
		localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
		appData := strings.TrimSpace(os.Getenv("APPDATA"))
		if localAppData != "" {
			resourceRoots = append(resourceRoots,
				filepath.Join(localAppData, "Programs", "Claude", "resources"),
				filepath.Join(localAppData, "AnthropicClaude", "resources"),
			)
		}
		if programFiles != "" {
			resourceRoots = append(resourceRoots, filepath.Join(programFiles, "Claude", "resources"))
		}
		if localAppData != "" {
			root := filepath.Join(localAppData, "Packages", "Claude_pzs8sxrjxfjjc", "LocalCache", "Roaming", "Claude", "claude-code")
			codeBinaries = append(codeBinaries, globFiles(filepath.Join(root, "*", "claude.exe"))...)
		}
		if appData != "" {
			root := filepath.Join(appData, "Claude", "claude-code")
			codeBinaries = append(codeBinaries, globFiles(filepath.Join(root, "*", "claude.exe"))...)
		}
	}

	sources := telemetryMaterialSources{}
	for _, root := range uniqueExistingPaths(resourceRoots, true) {
		archive := filepath.Join(root, "app.asar")
		if fileExists(archive) {
			sources.resourceArchives = append(sources.resourceArchives, archive)
		}
		for _, webRoot := range []string{filepath.Join(root, "ion-dist"), root} {
			if directoryExists(webRoot) {
				sources.webRoots = append(sources.webRoots, webRoot)
				break
			}
		}
	}
	sources.codeBinaries = uniqueExistingPaths(codeBinaries, false)
	return sources
}

func telemetryWebAssetPriority(path string) int {
	name := strings.ToLower(filepath.Base(path))
	switch {
	case strings.HasPrefix(name, "index-") || name == "index.js" || name == "index.html":
		return 0
	case strings.HasPrefix(name, "shared-"):
		return 1
	default:
		return 2
	}
}

func firstFileMatch(ctx context.Context, path string, pattern *regexp.Regexp, overlap int) ([][]byte, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, errOpen
	}
	defer func() { _ = file.Close() }()
	if overlap < 1024 {
		overlap = 1024
	}
	buffer := make([]byte, 256*1024)
	tail := make([]byte, 0, overlap)
	for {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		count, errRead := file.Read(buffer)
		if count > 0 {
			chunk := make([]byte, 0, len(tail)+count)
			chunk = append(chunk, tail...)
			chunk = append(chunk, buffer[:count]...)
			if match := pattern.FindSubmatch(chunk); len(match) > 0 {
				return match, nil
			}
			keep := min(overlap, len(chunk))
			tail = append(tail[:0], chunk[len(chunk)-keep:]...)
		}
		if errors.Is(errRead, io.EOF) {
			break
		}
		if errRead != nil {
			return nil, errRead
		}
	}
	return nil, os.ErrNotExist
}

func fileWindowAfter(ctx context.Context, path string, anchor []byte, windowSize int) ([]byte, error) {
	if len(anchor) == 0 || windowSize <= 0 {
		return nil, fmt.Errorf("telemetry material anchor is invalid")
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return nil, errOpen
	}
	defer func() { _ = file.Close() }()
	buffer := make([]byte, 256*1024)
	tail := make([]byte, 0, len(anchor)-1)
	for {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		count, errRead := file.Read(buffer)
		if count > 0 {
			chunk := make([]byte, 0, len(tail)+count)
			chunk = append(chunk, tail...)
			chunk = append(chunk, buffer[:count]...)
			if index := bytes.Index(chunk, anchor); index >= 0 {
				start := index + len(anchor)
				result := append([]byte(nil), chunk[start:]...)
				if len(result) < windowSize {
					additional := make([]byte, windowSize-len(result))
					read, _ := io.ReadFull(file, additional)
					result = append(result, additional[:read]...)
				}
				return result[:min(windowSize, len(result))], nil
			}
			keep := min(len(anchor)-1, len(chunk))
			tail = append(tail[:0], chunk[len(chunk)-keep:]...)
		}
		if errors.Is(errRead, io.EOF) {
			break
		}
		if errRead != nil {
			return nil, errRead
		}
	}
	return nil, os.ErrNotExist
}

func globFiles(pattern string) []string {
	paths, _ := filepath.Glob(pattern)
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if fileExists(path) {
			result = append(result, path)
		}
	}
	return result
}

func globDirectories(pattern string) []string {
	paths, _ := filepath.Glob(pattern)
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if directoryExists(path) {
			result = append(result, path)
		}
	}
	return result
}

func uniqueExistingPaths(paths []string, wantDirectory bool) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || path == "" {
			continue
		}
		absolute, errAbs := filepath.Abs(path)
		if errAbs != nil {
			continue
		}
		info, errStat := os.Stat(absolute)
		if errStat != nil || info.IsDir() != wantDirectory {
			continue
		}
		key := strings.ToLower(absolute)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, absolute)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] > result[right] })
	return result
}

func fileExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && !info.IsDir()
}

func directoryExists(path string) bool {
	info, errStat := os.Stat(path)
	return errStat == nil && info.IsDir()
}
