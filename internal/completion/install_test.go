package completion

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestInstallZshPreservesProfileAndIsIdempotent(t *testing.T) {
	homeDir := t.TempDir()
	configDir := filepath.Join(homeDir, ".config")
	profilePath := filepath.Join(homeDir, ".zshrc")
	if err := os.WriteFile(profilePath, []byte("export KEEP_ME=1\n"), 0o600); err != nil {
		t.Fatalf("write profile fixture: %v", err)
	}

	environment := Environment{
		HomeDir:   homeDir,
		ConfigDir: configDir,
		Shell:     "/bin/zsh",
		RuntimeOS: "darwin",
	}
	root := testRoot()

	first, err := Install(root, environment, "", "")
	if err != nil {
		t.Fatalf("install completion: %v", err)
	}
	second, err := Install(root, environment, "zsh", "")
	if err != nil {
		t.Fatalf("reinstall completion: %v", err)
	}
	if first.ScriptPath != second.ScriptPath {
		t.Fatalf("script path changed from %q to %q", first.ScriptPath, second.ScriptPath)
	}

	script := readFile(t, first.ScriptPath)
	if !strings.Contains(script, "#compdef sessionio") {
		t.Fatalf("completion script does not target sessionio:\n%s", script)
	}
	profile := readFile(t, profilePath)
	if !strings.HasPrefix(profile, "export KEEP_ME=1\n") {
		t.Fatalf("existing profile content was not preserved:\n%s", profile)
	}
	if strings.Count(profile, blockStart) != 1 || strings.Count(profile, blockEnd) != 1 {
		t.Fatalf("managed block is not idempotent:\n%s", profile)
	}
	info, err := os.Stat(profilePath)
	if err != nil {
		t.Fatalf("stat profile: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode = %o, want 600", info.Mode().Perm())
	}
}

func TestInstallFishUsesAutoloadDirectoryWithoutProfile(t *testing.T) {
	homeDir := t.TempDir()
	configDir := filepath.Join(homeDir, "config")
	result, err := Install(
		testRoot(),
		Environment{
			HomeDir:   homeDir,
			ConfigDir: configDir,
			Shell:     "/usr/bin/fish",
			RuntimeOS: "linux",
		},
		"",
		"",
	)
	if err != nil {
		t.Fatalf("install completion: %v", err)
	}

	wantPath := filepath.Join(configDir, "fish", "completions", "sessionio.fish")
	if result.ScriptPath != wantPath {
		t.Fatalf("script path = %q, want %q", result.ScriptPath, wantPath)
	}
	if len(result.ProfilePaths) != 0 {
		t.Fatalf("profile paths = %v, want none", result.ProfilePaths)
	}
	if !strings.Contains(readFile(t, result.ScriptPath), "sessionio") {
		t.Fatal("fish completion script does not mention sessionio")
	}
}

func TestInstallPowerShellRequiresKnownProfile(t *testing.T) {
	homeDir := t.TempDir()
	_, err := Install(
		testRoot(),
		Environment{
			HomeDir:   homeDir,
			ConfigDir: filepath.Join(homeDir, "config"),
			RuntimeOS: "windows",
		},
		"powershell",
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "PowerShell profile is unknown") {
		t.Fatalf("error = %v, want unknown profile error", err)
	}
}

func TestInstallPowerShellConnectsExplicitProfile(t *testing.T) {
	homeDir := t.TempDir()
	profilePath := filepath.Join(homeDir, "PowerShell", "profile.ps1")
	result, err := Install(
		testRoot(),
		Environment{
			HomeDir:   homeDir,
			ConfigDir: filepath.Join(homeDir, "config"),
			RuntimeOS: "windows",
		},
		"pwsh",
		profilePath,
	)
	if err != nil {
		t.Fatalf("install completion: %v", err)
	}
	if result.Shell != "powershell" {
		t.Fatalf("shell = %q, want powershell", result.Shell)
	}
	profile := readFile(t, profilePath)
	if !strings.Contains(profile, "Test-Path -LiteralPath") {
		t.Fatalf("PowerShell profile does not load completion:\n%s", profile)
	}
	if !strings.Contains(readFile(t, result.ScriptPath), "Register-ArgumentCompleter") {
		t.Fatal("PowerShell completion script does not register a completer")
	}
}

func TestInstallBashConnectsLoginAndInteractiveProfilesOnDarwin(t *testing.T) {
	homeDir := t.TempDir()
	result, err := Install(
		testRoot(),
		Environment{
			HomeDir:   homeDir,
			ConfigDir: filepath.Join(homeDir, "config"),
			RuntimeOS: "darwin",
		},
		"bash",
		"",
	)
	if err != nil {
		t.Fatalf("install completion: %v", err)
	}
	if len(result.ProfilePaths) != 2 {
		t.Fatalf("profile paths = %v, want bashrc and bash_profile", result.ProfilePaths)
	}
	for _, profilePath := range result.ProfilePaths {
		profile := readFile(t, profilePath)
		if !strings.Contains(profile, blockStart) {
			t.Fatalf("profile %s does not contain managed block", profilePath)
		}
	}
}

func TestInstallRejectsMalformedManagedBlock(t *testing.T) {
	homeDir := t.TempDir()
	profilePath := filepath.Join(homeDir, ".zshrc")
	if err := os.WriteFile(profilePath, []byte(blockStart+"\n"), 0o644); err != nil {
		t.Fatalf("write profile fixture: %v", err)
	}

	_, err := Install(
		testRoot(),
		Environment{
			HomeDir:   homeDir,
			ConfigDir: filepath.Join(homeDir, ".config"),
			Shell:     "zsh",
			RuntimeOS: "linux",
		},
		"",
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v, want malformed marker error", err)
	}
}

func TestDetectShellRejectsUnsupportedShell(t *testing.T) {
	_, err := detectShell("", Environment{Shell: "/bin/tcsh"})
	if err == nil || !strings.Contains(err.Error(), "unsupported shell") {
		t.Fatalf("error = %v, want unsupported shell error", err)
	}
}

func testRoot() *cobra.Command {
	root := &cobra.Command{Use: "sessionio"}
	root.AddCommand(&cobra.Command{
		Use: "example",
		Run: func(*cobra.Command, []string) {},
	})
	return root
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
