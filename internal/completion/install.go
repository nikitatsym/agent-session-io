package completion

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const (
	blockStart = "# >>> sessionio completion >>>"
	blockEnd   = "# <<< sessionio completion <<<"
)

// Environment describes the user directories relevant to shell completion.
type Environment struct {
	HomeDir           string
	ConfigDir         string
	ZDOTDir           string
	Shell             string
	RuntimeOS         string
	PowerShellProfile string
}

// Result describes installed completion files and modified profiles.
type Result struct {
	Shell        string
	ScriptPath   string
	ProfilePaths []string
}

// CurrentEnvironment reads completion paths from the current process.
func CurrentEnvironment() (Environment, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return Environment{}, fmt.Errorf("find home directory: %w", err)
	}
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		configDir, err = os.UserConfigDir()
		if err != nil {
			return Environment{}, fmt.Errorf("find user config directory: %w", err)
		}
	}
	return Environment{
		HomeDir:           homeDir,
		ConfigDir:         configDir,
		ZDOTDir:           os.Getenv("ZDOTDIR"),
		Shell:             os.Getenv("SHELL"),
		RuntimeOS:         runtime.GOOS,
		PowerShellProfile: os.Getenv("SESSIONIO_POWERSHELL_PROFILE"),
	}, nil
}

// Install generates and connects completion for one shell.
func Install(
	root *cobra.Command,
	environment Environment,
	requestedShell string,
	requestedProfile string,
) (Result, error) {
	shell, err := detectShell(requestedShell, environment)
	if err != nil {
		return Result{}, err
	}
	if environment.HomeDir == "" {
		return Result{}, fmt.Errorf("home directory is empty")
	}
	if environment.ConfigDir == "" {
		return Result{}, fmt.Errorf("user config directory is empty")
	}
	if !filepath.IsAbs(environment.HomeDir) {
		return Result{}, fmt.Errorf("home directory must be absolute: %s", environment.HomeDir)
	}
	if !filepath.IsAbs(environment.ConfigDir) {
		return Result{}, fmt.Errorf("user config directory must be absolute: %s", environment.ConfigDir)
	}

	result := Result{Shell: shell}
	switch shell {
	case "bash":
		result.ScriptPath = filepath.Join(
			environment.ConfigDir,
			"sessionio",
			"completions",
			"bash",
			"sessionio.bash",
		)
		result.ProfilePaths = bashProfiles(environment)
	case "fish":
		result.ScriptPath = filepath.Join(
			environment.ConfigDir,
			"fish",
			"completions",
			"sessionio.fish",
		)
	case "powershell":
		result.ScriptPath = filepath.Join(
			environment.ConfigDir,
			"sessionio",
			"completions",
			"powershell",
			"sessionio.ps1",
		)
		profile := requestedProfile
		if profile == "" {
			profile = environment.PowerShellProfile
		}
		if profile == "" {
			return Result{}, fmt.Errorf(
				"PowerShell profile is unknown; pass --profile or set SESSIONIO_POWERSHELL_PROFILE",
			)
		}
		result.ProfilePaths = []string{profile}
	case "zsh":
		result.ScriptPath = filepath.Join(
			environment.ConfigDir,
			"sessionio",
			"completions",
			"zsh",
			"_sessionio",
		)
		profileDir := environment.ZDOTDir
		if profileDir == "" {
			profileDir = environment.HomeDir
		}
		result.ProfilePaths = []string{filepath.Join(profileDir, ".zshrc")}
	default:
		return Result{}, fmt.Errorf("unsupported shell %q", shell)
	}

	if err := generate(root, shell, result.ScriptPath); err != nil {
		return Result{}, err
	}
	for _, profilePath := range result.ProfilePaths {
		if !filepath.IsAbs(profilePath) {
			return Result{}, fmt.Errorf("shell profile path must be absolute: %s", profilePath)
		}
		block := profileBlock(shell, result.ScriptPath)
		if err := ensureManagedBlock(profilePath, block); err != nil {
			return Result{}, fmt.Errorf("connect %s completion in %s: %w", shell, profilePath, err)
		}
	}
	return result, nil
}

func detectShell(requested string, environment Environment) (string, error) {
	value := requested
	if value == "" {
		value = filepath.Base(environment.Shell)
	}
	if value == "" && environment.RuntimeOS == "windows" {
		value = "powershell"
	}
	value = strings.ToLower(strings.TrimSuffix(value, ".exe"))
	switch value {
	case "bash", "fish", "zsh":
		return value, nil
	case "powershell", "pwsh":
		return "powershell", nil
	case "":
		return "", fmt.Errorf("cannot detect shell; pass bash, fish, powershell, or zsh")
	default:
		return "", fmt.Errorf("unsupported shell %q; pass bash, fish, powershell, or zsh", value)
	}
}

func bashProfiles(environment Environment) []string {
	profiles := []string{filepath.Join(environment.HomeDir, ".bashrc")}
	bashProfile := filepath.Join(environment.HomeDir, ".bash_profile")
	if environment.RuntimeOS == "darwin" {
		return append(profiles, bashProfile)
	}
	if _, err := os.Stat(bashProfile); err == nil {
		return append(profiles, bashProfile)
	}
	return profiles
}

func generate(root *cobra.Command, shell string, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create completion directory: %w", err)
	}
	var content bytes.Buffer
	var err error
	switch shell {
	case "bash":
		err = root.GenBashCompletionV2(&content, true)
	case "fish":
		err = root.GenFishCompletion(&content, true)
	case "powershell":
		err = root.GenPowerShellCompletionWithDesc(&content)
	case "zsh":
		err = root.GenZshCompletion(&content)
	default:
		return fmt.Errorf("unsupported shell %q", shell)
	}
	if err != nil {
		return fmt.Errorf("generate %s completion: %w", shell, err)
	}
	if err := os.WriteFile(path, content.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write %s completion to %s: %w", shell, path, err)
	}
	return nil
}

func profileBlock(shell string, scriptPath string) string {
	switch shell {
	case "bash":
		quoted := quotePOSIX(scriptPath)
		return fmt.Sprintf(
			"%s\nif [ -r %s ]; then\n    . %s\nfi\n%s",
			blockStart,
			quoted,
			quoted,
			blockEnd,
		)
	case "powershell":
		quoted := quotePowerShell(scriptPath)
		return fmt.Sprintf(
			"%s\nif (Test-Path -LiteralPath %s) {\n    . %s\n}\n%s",
			blockStart,
			quoted,
			quoted,
			blockEnd,
		)
	case "zsh":
		quoted := quotePOSIX(scriptPath)
		return fmt.Sprintf(
			"%s\n(( $+functions[compdef] )) || { autoload -Uz compinit && compinit }\nsource %s\n%s",
			blockStart,
			quoted,
			blockEnd,
		)
	default:
		panic(fmt.Sprintf("profile block requested for unsupported shell %q", shell))
	}
}

func ensureManagedBlock(path string, block string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read profile: %w", err)
	}

	startCount := bytes.Count(content, []byte(blockStart))
	endCount := bytes.Count(content, []byte(blockEnd))
	if startCount != endCount || startCount > 1 {
		return fmt.Errorf("profile contains malformed sessionio completion markers")
	}

	var updated []byte
	if startCount == 0 {
		updated = append(updated, content...)
		if len(updated) > 0 && updated[len(updated)-1] != '\n' {
			updated = append(updated, '\n')
		}
		if len(updated) > 0 {
			updated = append(updated, '\n')
		}
		updated = append(updated, block...)
		updated = append(updated, '\n')
	} else {
		start := bytes.Index(content, []byte(blockStart))
		endRelative := bytes.Index(content[start:], []byte(blockEnd))
		if endRelative < 0 {
			return fmt.Errorf("profile contains malformed sessionio completion markers")
		}
		end := start + endRelative + len(blockEnd)
		updated = append(updated, content[:start]...)
		updated = append(updated, block...)
		updated = append(updated, content[end:]...)
	}
	if bytes.Equal(content, updated) {
		return nil
	}
	if err := writeProfile(path, updated); err != nil {
		return err
	}
	return nil
}

func writeProfile(path string, content []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat profile: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".sessionio-profile-*")
	if err != nil {
		return fmt.Errorf("create temporary profile: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set temporary profile mode: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary profile: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary profile: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary profile: %w", err)
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return fmt.Errorf("replace profile: %w", err)
	}
	return nil
}

func replaceFile(source string, destination string) error {
	if err := os.Rename(source, destination); err == nil {
		return nil
	}
	backup, err := os.CreateTemp(filepath.Dir(destination), ".sessionio-profile-backup-*")
	if err != nil {
		return err
	}
	backupPath := backup.Name()
	if err := backup.Close(); err != nil {
		return err
	}
	if err := os.Remove(backupPath); err != nil {
		return err
	}
	defer os.Remove(backupPath)

	if err := os.Rename(destination, backupPath); err != nil {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		if rollbackErr := os.Rename(backupPath, destination); rollbackErr != nil {
			return fmt.Errorf("%w; rollback failed: %v", err, rollbackErr)
		}
		return err
	}
	return os.Remove(backupPath)
}

func quotePOSIX(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func quotePowerShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
