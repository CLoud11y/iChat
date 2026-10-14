package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func runtimeConfigFile(t *testing.T, yaml string) string {
	t.Helper()
	path := mqConfigFile(t, yaml)
	for _, name := range []string{"ICHAT_RUNTIME", "ICHAT_RUNTIME_MODE"} {
		old, present := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(name, old)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	// Viper treats an empty legacy env override as omitted, leaving fixture YAML
	// or its default in control rather than inheriting an operator's worker flag.
	t.Setenv("ICHAT_LEGACY_WORKER_ENABLED", "")
	return path
}

func TestRuntimePlanSeparatesProcessRoles(t *testing.T) {
	for _, tc := range []struct {
		mode                              RuntimeMode
		api, publisher, consumers, outbox bool
	}{
		{ModeLegacy, true, false, false, false}, {ModeAPI, true, true, false, false},
		{ModeWorker, false, false, true, true}, {ModeAll, true, true, true, true},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			var c Config
			c.RUNTIME.Mode = tc.mode
			c.MYSQL.Host = "must-not-resolve"
			c.RABBITMQ.URL = "SECRET: role planning must not interpret transport credentials"
			before := c
			plan, err := c.RuntimePlan()
			if err != nil {
				t.Fatal(err)
			}
			if plan.Mode != tc.mode || plan.API != tc.api || plan.Publisher != tc.publisher || plan.PersistenceConsumer != tc.consumers ||
				plan.QuarantineConsumer != tc.consumers || plan.Outbox != tc.outbox || plan.LegacyWorker {
				t.Fatal("runtime selected unintended API/publisher/consumer/legacy roles")
			}
			if c != before {
				t.Fatal("role planning mutated configuration")
			}
		})
	}
	var zero Config
	plan, err := zero.RuntimePlan()
	if err != nil || plan.Mode != ModeLegacy || zero.RUNTIME.Mode != "" {
		t.Fatal("direct zero config lost legacy compatibility", err)
	}
	zero.LEGACY_WORKER.Enabled = true
	plan, err = zero.RuntimePlan()
	if err != nil || !plan.LegacyWorker || plan.Publisher || plan.PersistenceConsumer {
		t.Fatal("legacy worker was mixed with new transport roles", err)
	}
}

func TestRuntimeLoadSourcesAndIndependentReload(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		hasEnv          bool
		mode            RuntimeMode
	}{
		{"omitted", "APP:\n  name: test\n", "", false, ModeLegacy},
		{"legacy", "RUNTIME:\n  mode: legacy\n", "", false, ModeLegacy},
		{"api", "RUNTIME:\n  mode: api\n", "", false, ModeAPI},
		{"worker", "RUNTIME:\n  mode: worker\n", "", false, ModeWorker},
		{"all", "RUNTIME:\n  mode: all\n", "", false, ModeAll},
		{"case_insensitive_key", "RUNTIME:\n  MODE: worker\n", "", false, ModeWorker},
		{"env_without_yaml", "APP:\n  name: test\n", "api", true, ModeAPI},
		{"env_overrides_yaml", "RUNTIME:\n  mode: all\n", "worker", true, ModeWorker},
		{"env_explicit_legacy", "RUNTIME:\n  mode: api\n", "legacy", true, ModeLegacy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimeConfigFile(t, tc.yaml)
			if tc.hasEnv {
				t.Setenv("ICHAT_RUNTIME_MODE", tc.env)
			}
			c, err := Load()
			if err != nil || c.RUNTIME.Mode != tc.mode {
				t.Fatalf("mode=%q want=%q error=%v", c.RUNTIME.Mode, tc.mode, err)
			}
			if _, err := c.RuntimePlan(); err != nil {
				t.Fatal(err)
			}
		})
	}
	path := runtimeConfigFile(t, "RUNTIME:\n  mode: worker\n")
	if c, err := Load(); err != nil || c.RUNTIME.Mode != ModeWorker {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("APP:\n  name: legacy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(); err != nil || c.RUNTIME.Mode != ModeLegacy {
		t.Fatal("reload retained previous mode", err)
	}
	if Conf != (Config{}) {
		t.Fatal("Load/plan assigned legacy globals")
	}
}

func TestRuntimeInvalidAndMixedLegacyInputsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		hasEnv          bool
	}{
		{"empty_yaml", "RUNTIME:\n  mode: ''\n", "", false},
		{"empty_env_override", "RUNTIME:\n  mode: legacy\n", "", true},
		{"secret_env", "APP:\n  name: test\n", "SECRET_TOKEN", true},
		{"unknown_mode", "RUNTIME:\n  mode: experimental\n", "", false},
		{"uppercase_mode", "RUNTIME:\n  mode: API\n", "", false},
		{"whitespace", "APP:\n  name: test\n", " api ", true},
		{"null_mode", "RUNTIME:\n  mode: null\n", "", false},
		{"numeric_mode", "RUNTIME:\n  mode: 1\n", "", false},
		{"boolean_mode", "RUNTIME:\n  mode: true\n", "", false},
		{"object_mode", "RUNTIME:\n  mode: {token: SECRET_TOKEN}\n", "", false},
		{"scalar_section", "RUNTIME: SECRET_TOKEN\n", "", false},
		{"unknown_key", "RUNTIME:\n  mode: api\n  token: SECRET_TOKEN\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimeConfigFile(t, tc.yaml)
			if tc.hasEnv {
				t.Setenv("ICHAT_RUNTIME_MODE", tc.env)
			}
			c, err := Load()
			if !errors.Is(err, ErrRuntimeConfig) || c != (Config{}) {
				t.Fatalf("unsafe runtime accepted: %v", err)
			}
			if strings.Contains(err.Error(), "SECRET_TOKEN") {
				t.Fatal("runtime error echoed input")
			}
		})
	}
	for _, mode := range []RuntimeMode{ModeAPI, ModeWorker, ModeAll} {
		t.Run(string(mode)+"_with_legacy_worker", func(t *testing.T) {
			runtimeConfigFile(t, "RUNTIME:\n  mode: "+string(mode)+"\nLEGACY_WORKER:\n  enabled: true\n")
			if c, err := Load(); !errors.Is(err, ErrRuntimeConfig) || c != (Config{}) {
				t.Fatal("mixed legacy/new roles were loaded", err)
			}
			var direct Config
			direct.RUNTIME.Mode, direct.LEGACY_WORKER.Enabled = mode, true
			if plan, err := direct.RuntimePlan(); !errors.Is(err, ErrRuntimeConfig) || plan != (RuntimePlan{}) {
				t.Fatal("direct conflicting config bypassed validation", err)
			}
		})
	}
}
