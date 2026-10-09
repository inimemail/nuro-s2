package setup

import (
	"os"
	"strings"
	"sync"
	"testing"
)

func TestSetupConfigKeepsFirstPublishedSecret(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	first := &SetupConfig{JWT: JWTConfig{Secret: strings.Repeat("a", 64)}}
	second := &SetupConfig{JWT: JWTConfig{Secret: strings.Repeat("b", 64)}}
	if err := writeConfigFile(first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(GetConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfigFile(second); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(GetConfigFilePath())
	if err != nil || string(after) != string(before) {
		t.Fatalf("a competing setup replaced the published configuration: %v", err)
	}
}

func TestSharedSetupFilesConcurrentPublish(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	const replicas = 16
	var wg sync.WaitGroup
	errors := make(chan error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		secret := strings.Repeat(string(rune('a'+i)), 4096)
		go func() {
			defer wg.Done()
			if err := writeConfigFile(&SetupConfig{JWT: JWTConfig{Secret: secret}}); err != nil {
				errors <- err
				return
			}
			errors <- createInstallLock()
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(GetInstallLockPath())
	if err != nil || !strings.HasPrefix(string(before), "installed_at=") {
		t.Fatalf("invalid installation marker: %v", err)
	}
	if err := createInstallLock(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(GetInstallLockPath())
	if string(before) != string(after) {
		t.Fatal("existing installation marker was replaced")
	}
	for path, expected := range map[string]os.FileMode{GetConfigFilePath(): 0600, GetInstallLockPath(): 0400} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != expected {
			t.Fatalf("unexpected permissions for %s: %v", path, err)
		}
	}
	data, err := os.ReadFile(GetConfigFilePath())
	completeSecret := false
	for i := 0; i < replicas; i++ {
		completeSecret = completeSecret || strings.Contains(string(data), strings.Repeat(string(rune('a'+i)), 4096))
	}
	if err != nil || !completeSecret {
		t.Fatalf("partial config file: %v", err)
	}
	files, err := os.ReadDir(GetDataDir())
	if err != nil || len(files) != 2 {
		t.Fatalf("temporary setup files leaked: %v", err)
	}
}

func TestDecideAdminBootstrap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		totalUsers int64
		adminUsers int64
		should     bool
		reason     string
	}{
		{
			name:       "empty database should create admin",
			totalUsers: 0,
			adminUsers: 0,
			should:     true,
			reason:     adminBootstrapReasonEmptyDatabase,
		},
		{
			name:       "admin exists should skip",
			totalUsers: 10,
			adminUsers: 1,
			should:     false,
			reason:     adminBootstrapReasonAdminExists,
		},
		{
			name:       "users exist without admin should skip",
			totalUsers: 5,
			adminUsers: 0,
			should:     false,
			reason:     adminBootstrapReasonUsersExistWithoutAdmin,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideAdminBootstrap(tc.totalUsers, tc.adminUsers)
			if got.shouldCreate != tc.should {
				t.Fatalf("shouldCreate=%v, want %v", got.shouldCreate, tc.should)
			}
			if got.reason != tc.reason {
				t.Fatalf("reason=%q, want %q", got.reason, tc.reason)
			}
		})
	}
}

func TestSetupDefaultAdminConcurrency(t *testing.T) {
	t.Run("simple mode admin uses higher concurrency", func(t *testing.T) {
		t.Setenv("RUN_MODE", "simple")
		if got := setupDefaultAdminConcurrency(); got != simpleModeAdminConcurrency {
			t.Fatalf("setupDefaultAdminConcurrency()=%d, want %d", got, simpleModeAdminConcurrency)
		}
	})

	t.Run("standard mode keeps existing default", func(t *testing.T) {
		t.Setenv("RUN_MODE", "standard")
		if got := setupDefaultAdminConcurrency(); got != defaultUserConcurrency {
			t.Fatalf("setupDefaultAdminConcurrency()=%d, want %d", got, defaultUserConcurrency)
		}
	})
}

func TestNeedsSetupHonorsExplicitBypass(t *testing.T) {
	for _, value := range []string{"true", "1", "yes", "  TrUe  ", " YeS "} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("DATA_DIR", t.TempDir())
			t.Setenv("SKIP_SETUP", value)
			if NeedsSetup() {
				t.Fatal("NeedsSetup() = true, want false")
			}
		})
	}
}

func TestNeedsSetupIgnoresUnknownBypassValues(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	t.Setenv("SKIP_SETUP", "enabled")
	if !NeedsSetup() {
		t.Fatal("NeedsSetup() = false, want true")
	}
}

func TestWriteConfigFileKeepsDefaultUserConcurrency(t *testing.T) {
	t.Setenv("RUN_MODE", "simple")
	t.Setenv("DATA_DIR", t.TempDir())

	if err := writeConfigFile(&SetupConfig{}); err != nil {
		t.Fatalf("writeConfigFile() error = %v", err)
	}

	data, err := os.ReadFile(GetConfigFilePath())
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	if !strings.Contains(string(data), "user_concurrency: 5") {
		t.Fatalf("config missing default user concurrency, got:\n%s", string(data))
	}
}
