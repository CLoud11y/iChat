package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

var ErrRuntimeConfig = errors.New("invalid application runtime configuration")

type RuntimeMode string

const (
	ModeLegacy RuntimeMode = "legacy"
	ModeAPI    RuntimeMode = "api"
	ModeWorker RuntimeMode = "worker"
	ModeAll    RuntimeMode = "all"
)

type RuntimeConfig struct {
	Mode RuntimeMode
}

// RuntimePlan selects process roles, not active services or health status.
// The assembly layer must implement every selected role before starting one.
type RuntimePlan struct {
	Mode                RuntimeMode
	API                 bool
	Publisher           bool
	PersistenceConsumer bool
	QuarantineConsumer  bool
	Outbox              bool
	LegacyWorker        bool
}

// RuntimePlan performs no I/O, changes no config/global state, and also checks
// directly constructed configs. An omitted Go field preserves legacy behavior;
// an explicitly empty YAML/environment mode is rejected by loadRuntime.
func (c Config) RuntimePlan() (RuntimePlan, error) {
	mode := c.RUNTIME.Mode
	if mode == "" {
		mode = ModeLegacy
	}
	switch mode {
	case ModeLegacy:
		return RuntimePlan{Mode: mode, API: true, LegacyWorker: c.LEGACY_WORKER.Enabled}, nil
	case ModeAPI, ModeWorker, ModeAll:
		if c.LEGACY_WORKER.Enabled {
			return RuntimePlan{}, fmt.Errorf("%w: LEGACY_WORKER.enabled conflicts with RUNTIME.mode", ErrRuntimeConfig)
		}
		return RuntimePlan{
			Mode: mode, API: mode != ModeWorker, Publisher: mode != ModeWorker,
			PersistenceConsumer: mode != ModeAPI, QuarantineConsumer: mode != ModeAPI, Outbox: mode != ModeAPI,
		}, nil
	default:
		return RuntimePlan{}, fmt.Errorf("%w: RUNTIME.mode", ErrRuntimeConfig)
	}
}

func loadRuntime(v *viper.Viper) (RuntimeConfig, error) {
	var section map[string]interface{}
	if v.IsSet("RUNTIME") {
		var ok bool
		section, ok = v.Get("RUNTIME").(map[string]interface{})
		if !ok {
			return RuntimeConfig{}, ErrRuntimeConfig
		}
		for key := range section {
			if strings.ToLower(key) != "mode" {
				return RuntimeConfig{}, fmt.Errorf("%w: unknown RUNTIME field", ErrRuntimeConfig)
			}
		}
	}
	var value any = string(ModeLegacy)
	if mode, ok := section["mode"]; ok {
		value = mode
	}
	if override, present := os.LookupEnv("ICHAT_RUNTIME_MODE"); present {
		value = override
	}
	mode, ok := value.(string)
	if !ok || (mode != string(ModeLegacy) && mode != string(ModeAPI) && mode != string(ModeWorker) && mode != string(ModeAll)) {
		return RuntimeConfig{}, fmt.Errorf("%w: RUNTIME.mode", ErrRuntimeConfig)
	}
	return RuntimeConfig{Mode: RuntimeMode(mode)}, nil
}
