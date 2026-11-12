package maintenance

import (
	"encoding/json"
	"testing"
)

func TestComposeNativeHelperRejectsBuildDefinitionsBeforeStoppingWriters(t *testing.T) {
	for _, mode := range []string{"all-in-one", "stack"} {
		t.Run(mode, func(t *testing.T) {
			cfg := environmentConfig(t)
			cfg.Mode = mode
			fake := &fakeComposeDocker{t: t, cfg: cfg}
			fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "old", "old", "synthetic-old-secret"})
			api := "api"
			if mode == "all-in-one" {
				api = "lanqin-email"
			}
			service := fake.deployment.Services[api]
			service.Build = json.RawMessage(`{"context":"/unselected/build-context"}`)
			fake.deployment.Services[api] = service
			controller := fake.controller()
			if err := controller.Preflight(t.Context(), cfg); err == nil {
				t.Fatal("native helper could implicitly build an image")
			}
			for _, call := range fake.calls {
				if call[0] == "compose" && call[len(composePrefix(cfg))] != "config" {
					t.Fatal("unsupported build matrix touched live services")
				}
			}
		})
	}
}
