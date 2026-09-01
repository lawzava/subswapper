package subswapper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var claudeConflictingEnvironment = map[string]struct{}{
	// Direct Anthropic API, profile, and workload federation credentials.
	"ANTHROPIC_API_KEY":             {},
	"ANTHROPIC_AUTH_TOKEN":          {},
	"ANTHROPIC_BASE_URL":            {},
	"ANTHROPIC_CONFIG_DIR":          {},
	"ANTHROPIC_CUSTOM_HEADERS":      {},
	"ANTHROPIC_FEDERATION_RULE_ID":  {},
	"ANTHROPIC_IDENTITY_TOKEN":      {},
	"ANTHROPIC_IDENTITY_TOKEN_FILE": {},
	"ANTHROPIC_ORGANIZATION_ID":     {},
	"ANTHROPIC_PROFILE":             {},
	"ANTHROPIC_SERVICE_ACCOUNT_ID":  {},
	"ANTHROPIC_WORKSPACE_ID":        {},

	// A setup token is fixed for a process. Provisioning inputs must not rotate it.
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN": {},
	"CLAUDE_CODE_OAUTH_SCOPES":        {},

	// Amazon Bedrock, Mantle, and the AWS default credential chain.
	"CLAUDE_CODE_USE_BEDROCK":                {},
	"CLAUDE_CODE_USE_MANTLE":                 {},
	"CLAUDE_CODE_SKIP_BEDROCK_AUTH":          {},
	"CLAUDE_CODE_SKIP_MANTLE_AUTH":           {},
	"CLAUDE_CODE_SKIP_AWS_CRED_CACHE":        {},
	"AWS_BEARER_TOKEN_BEDROCK":               {},
	"AWS_ACCESS_KEY_ID":                      {},
	"AWS_SECRET_ACCESS_KEY":                  {},
	"AWS_SESSION_TOKEN":                      {},
	"AWS_PROFILE":                            {},
	"AWS_DEFAULT_PROFILE":                    {},
	"AWS_REGION":                             {},
	"AWS_DEFAULT_REGION":                     {},
	"AWS_SHARED_CREDENTIALS_FILE":            {},
	"AWS_CONFIG_FILE":                        {},
	"AWS_WEB_IDENTITY_TOKEN_FILE":            {},
	"AWS_ROLE_ARN":                           {},
	"AWS_ROLE_SESSION_NAME":                  {},
	"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": {},
	"AWS_CONTAINER_CREDENTIALS_FULL_URI":     {},
	"AWS_CONTAINER_AUTHORIZATION_TOKEN":      {},
	"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": {},
	"AWS_EC2_METADATA_DISABLED":              {},
	"AWS_EC2_METADATA_SERVICE_ENDPOINT":      {},
	"AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE": {},
	"ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION":  {},

	// Google Cloud's Agent Platform route and default credential chain.
	"CLAUDE_CODE_USE_VERTEX":         {},
	"CLAUDE_CODE_SKIP_VERTEX_AUTH":   {},
	"GOOGLE_APPLICATION_CREDENTIALS": {},
	"GOOGLE_CLOUD_PROJECT":           {},
	"GCLOUD_PROJECT":                 {},
	"CLOUD_ML_REGION":                {},

	// Microsoft Foundry route and Azure's default credential chain.
	"CLAUDE_CODE_USE_FOUNDRY":             {},
	"CLAUDE_CODE_SKIP_FOUNDRY_AUTH":       {},
	"AZURE_CLIENT_ID":                     {},
	"AZURE_TENANT_ID":                     {},
	"AZURE_CLIENT_SECRET":                 {},
	"AZURE_CLIENT_CERTIFICATE_PATH":       {},
	"AZURE_CLIENT_CERTIFICATE_PASSWORD":   {},
	"AZURE_CLIENT_SEND_CERTIFICATE_CHAIN": {},
	"AZURE_USERNAME":                      {},
	"AZURE_PASSWORD":                      {},
	"AZURE_AUTHORITY_HOST":                {},
	"AZURE_FEDERATED_TOKEN_FILE":          {},
	"AZURE_POD_IDENTITY_AUTHORITY_HOST":   {},
	"AZURE_TOKEN_CREDENTIALS":             {},
	"MSI_ENDPOINT":                        {},
	"MSI_SECRET":                          {},
	"IDENTITY_ENDPOINT":                   {},
	"IDENTITY_HEADER":                     {},
	"IMDS_ENDPOINT":                       {},
}

var claudeConflictingEnvironmentPrefixes = []string{
	"ANTHROPIC_AWS_",
	"ANTHROPIC_BEDROCK_",
	"ANTHROPIC_VERTEX_",
	"VERTEX_REGION_CLAUDE_",
	"ANTHROPIC_FOUNDRY_",
	"SUBSWAPPER_",
}

// BuildClaudeLaunchEnvironment returns a new environment for one Claude
// account. Metadata keys must use the SUBSWAPPER_ namespace, and their values
// must not contain secrets.
func BuildClaudeLaunchEnvironment(base []string, configDir, setupToken string, metadata map[string]string) ([]string, error) {
	return buildClaudeLaunchEnvironment(base, configDir, setupToken, metadata, false)
}

func buildClaudeLaunchEnvironment(base []string, configDir, setupToken string, metadata map[string]string, inheritConfigDir bool) ([]string, error) {
	if !inheritConfigDir && (strings.TrimSpace(configDir) == "" || strings.IndexByte(configDir, 0) >= 0) {
		return nil, errors.New("claude account config directory is invalid")
	}
	if strings.TrimSpace(setupToken) == "" || strings.IndexByte(setupToken, 0) >= 0 {
		return nil, errors.New("claude setup token is invalid")
	}

	overrides := map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN":          setupToken,
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB": "1",
	}
	if !inheritConfigDir {
		overrides["CLAUDE_CONFIG_DIR"] = configDir
	}
	for key, value := range metadata {
		if !strings.HasPrefix(key, "SUBSWAPPER_") || strings.ContainsAny(key, "=\x00") || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("claude launch metadata is invalid")
		}
		overrides[key] = value
	}

	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			result = append(result, entry)
			continue
		}
		if claudeEnvironmentConflicts(key) {
			continue
		}
		if _, replaced := overrides[key]; replaced {
			continue
		}
		result = append(result, entry)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result, nil
}

// BuildClaudeProxyLaunchEnvironment routes a process through the local auth
// proxy. The process carries only the proxy secret; real tokens stay with the
// proxy, which selects the account for every request. An empty configDir
// leaves CLAUDE_CONFIG_DIR untouched so Claude uses its native home.
func BuildClaudeProxyLaunchEnvironment(base []string, configDir, proxySecret, listen string, metadata map[string]string) ([]string, error) {
	if err := validateLoopbackListen(listen); err != nil {
		return nil, fmt.Errorf("claude proxy listen address is invalid: %w", err)
	}
	filtered := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if key == "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL" {
			continue
		}
		filtered = append(filtered, entry)
	}
	environment, err := buildClaudeLaunchEnvironment(filtered, configDir, proxySecret, metadata, configDir == "")
	if err != nil {
		return nil, err
	}
	return append(environment,
		"ANTHROPIC_BASE_URL="+ClaudeProxyBaseURL(listen),
		"_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1",
	), nil
}

func claudeEnvironmentConflicts(key string) bool {
	if _, conflicts := claudeConflictingEnvironment[key]; conflicts {
		return true
	}
	for _, prefix := range claudeConflictingEnvironmentPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func scrubClaudeSubprocessEnvironment(base []string) []string {
	result := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (key == "CLAUDE_CODE_OAUTH_TOKEN" || claudeEnvironmentConflicts(key)) {
			continue
		}
		result = append(result, entry)
	}
	return result
}

type claudeConfigRewrite struct {
	path string
	data []byte
}

// PrepareClaudeAccountHome removes cached organization identity from the two
// Claude config filenames used by account homes. It never opens or writes a
// native Claude config file.
func PrepareClaudeAccountHome(accountHome string) error {
	return prepareClaudeRuntimeHome(accountHome, false)
}

// PrepareClaudeSharedRuntimeHome preserves all stored authentication while
// securing the credentials file shared by multiple setup-token launches.
func PrepareClaudeSharedRuntimeHome(runtimeHome string) error {
	return prepareClaudeRuntimeHome(runtimeHome, true)
}

func prepareClaudeRuntimeHome(accountHome string, secureCredentials bool) error {
	accountPath, err := canonicalPathWithMissingLeaf(accountHome)
	if err != nil {
		return fmt.Errorf("resolve Claude account home: %w", err)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return errors.New("cannot verify native Claude home")
	}
	nativeUserPath, err := canonicalPathWithMissingLeaf(userHome)
	if err != nil {
		return errors.New("cannot verify native Claude home")
	}
	nativeClaudePath, err := canonicalPathWithMissingLeaf(filepath.Join(userHome, ".claude"))
	if err != nil {
		return errors.New("cannot verify native Claude home")
	}
	if accountPath == nativeUserPath || pathsOverlap(accountPath, nativeClaudePath) {
		return errors.New("selected account home overlaps the native Claude home")
	}

	if err := os.MkdirAll(accountHome, 0o700); err != nil {
		return fmt.Errorf("create Claude account home: %w", err)
	}
	if err := os.Chmod(accountHome, 0o700); err != nil {
		return fmt.Errorf("secure Claude account home: %w", err)
	}

	rewrites := make([]claudeConfigRewrite, 0, 3)
	for _, name := range []string{".claude.json", ".config.json"} {
		path := filepath.Join(accountHome, name)
		data, mode, exists, err := readRegularClaudeConfig(path)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		var config map[string]json.RawMessage
		if err := json.Unmarshal(data, &config); err != nil || config == nil {
			return fmt.Errorf("%s contains malformed JSON", name)
		}
		_, hadOAuthAccount := config["oauthAccount"]
		if hadOAuthAccount {
			delete(config, "oauthAccount")
			data, err = json.MarshalIndent(config, "", "  ")
			if err != nil {
				return fmt.Errorf("encode %s: %w", name, err)
			}
		}
		if hadOAuthAccount || mode.Perm() != 0o600 {
			rewrites = append(rewrites, claudeConfigRewrite{path: path, data: data})
		}
	}
	if secureCredentials {
		path := filepath.Join(accountHome, ".credentials.json")
		data, mode, exists, err := readRegularClaudeConfig(path)
		if err != nil {
			return err
		}
		if exists && mode.Perm() != 0o600 {
			rewrites = append(rewrites, claudeConfigRewrite{path: path, data: data})
		}
	}
	return commitClaudeConfigRewrites(rewrites)
}

func readRegularClaudeConfig(path string) ([]byte, os.FileMode, bool, error) {
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect %s: %w", filepath.Base(path), err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect opened %s: %w", filepath.Base(path), err)
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return nil, 0, false, fmt.Errorf("%s is not a stable regular file", filepath.Base(path))
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return data, pathInfo.Mode(), true, nil
}

func commitClaudeConfigRewrites(rewrites []claudeConfigRewrite) error {
	staged := make([]stagedFile, 0, len(rewrites))
	for _, rewrite := range rewrites {
		file, err := stageClaudeConfigRewrite(rewrite)
		if err != nil {
			discardStagedFiles(staged)
			return err
		}
		staged = append(staged, file)
	}
	defer discardStagedFiles(staged)
	for _, file := range staged {
		if err := file.commit(); err != nil {
			return fmt.Errorf("replace %s: %w", filepath.Base(file.target), err)
		}
	}
	return nil
}

// stageClaudeConfigRewrite does not resolve the target through a symlink.
// Rename replaces a raced symlink instead of writing through it.
func stageClaudeConfigRewrite(rewrite claudeConfigRewrite) (stagedFile, error) {
	tmp, err := os.CreateTemp(filepath.Dir(rewrite.path), ".subswapper-claude-config-*")
	if err != nil {
		return stagedFile{}, fmt.Errorf("stage %s: %w", filepath.Base(rewrite.path), err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, bytes.NewReader(rewrite.data)); err != nil {
		return stagedFile{}, fmt.Errorf("stage %s: %w", filepath.Base(rewrite.path), err)
	}
	if err := tmp.Sync(); err != nil {
		return stagedFile{}, fmt.Errorf("sync %s: %w", filepath.Base(rewrite.path), err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return stagedFile{}, fmt.Errorf("secure %s: %w", filepath.Base(rewrite.path), err)
	}
	if err := tmp.Close(); err != nil {
		return stagedFile{}, fmt.Errorf("close %s: %w", filepath.Base(rewrite.path), err)
	}
	keep = true
	return stagedFile{tmpPath: tmpPath, target: rewrite.path}, nil
}

func canonicalPathWithMissingLeaf(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	ancestor := abs
	missing := make([]string, 0, 4)
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return abs, nil
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
		resolved, err := filepath.EvalSymlinks(ancestor)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		for i := len(missing) - 1; i >= 0; i-- {
			resolved = filepath.Join(resolved, missing[i])
		}
		return filepath.Clean(resolved), nil
	}
}

func pathsOverlap(first, second string) bool {
	return pathContains(first, second) || pathContains(second, first)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
