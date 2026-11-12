package maintenance

import (
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestComposePGOptionsAffectCompleteURLWithoutAnyDatabaseConnection(t *testing.T) {
	// Never discover a user's service or password files during this offline
	// proof. All possible file references are explicitly synthetic/unavailable.
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGSERVICEFILE", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "synthetic-absent-pgpass"))
	t.Setenv("PGOPTIONS", "-csearch_path=alternate -crole=alternate_role")
	parsed, err := pgx.ParseConfig("postgres://synthetic:synthetic-password@database:5432/new?sslmode=disable")
	if err != nil || parsed.Database != "new" || parsed.User != "synthetic" || parsed.RuntimeParams["options"] != "-csearch_path=alternate -crole=alternate_role" {
		t.Fatal("complete API URL no longer demonstrates the environment startup override", err)
	}
}

func TestComposePGSessionOverridesRejectedInEveryRenderedApplicationRole(t *testing.T) {
	values := map[string]string{"PGOPTIONS": "-csearch_path=alternate", "PGSERVICE": "synthetic-service", "PGSERVICEFILE": "/unused/synthetic-service.conf", "PGHOSTADDR": "192.0.2.34"}
	for _, mode := range []string{"all-in-one", "stack"} {
		cfg := environmentConfig(t)
		cfg.Mode = mode
		roles := []string{"api", "postfix", "dovecot", "rspamd"}
		if mode == "all-in-one" {
			roles = []string{"lanqin-email"}
		}
		for _, role := range roles {
			for key, value := range values {
				t.Run(mode+"/"+role+"/"+key, func(t *testing.T) {
					fake := &fakeComposeDocker{t: t, cfg: cfg}
					fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "new", "new", "synthetic-next-secret"})
					service := fake.deployment.Services[role]
					// Official deployment variables remain supported.
					service.Environment["LANQIN_POSTGRES_DATABASE"] = composeString("new")
					fake.deployment.Services[role] = service
					if err := validateComposeRuntime(cfg, fake.deployment, true); err != nil {
						t.Fatal("official LANQIN_POSTGRES variables were rejected", err)
					}
					service.Environment[key] = composeString(value)
					if err := validateComposeRuntime(cfg, fake.deployment, true); err == nil {
						t.Fatal("external session/service override made API/mail identity unprovable")
					}
					service.Environment[key] = composeString("")
					if err := validateComposeRuntime(cfg, fake.deployment, true); err != nil {
						t.Fatal("explicitly empty override was not ignored", err)
					}
				})
			}
		}
	}
}

func TestComposePGImageDefaultsCannotAddHiddenSessionOrServiceOverrides(t *testing.T) {
	for _, role := range []string{"api", "postfix", "dovecot", "rspamd"} {
		for _, key := range []string{"PGOPTIONS", "PGSERVICE", "PGSERVICEFILE", "PGHOSTADDR"} {
			t.Run(role+"/"+key, func(t *testing.T) {
				cfg := environmentConfig(t)
				cfg.Mode = "stack"
				fake := &fakeComposeDocker{t: t, cfg: cfg}
				fake.setDeployment("postgres", composeDatabaseIdentity{"database", "5432", "old", "old", "synthetic-old-secret"})
				for i, container := range fake.containers {
					if container.Config.Labels["com.docker.compose.service"] == role {
						fake.containers[i].Config.Env = append(fake.containers[i].Config.Env, key+"=synthetic-hidden-image-override")
					}
				}
				controller := fake.controller()
				if err := controller.Preflight(t.Context(), cfg); err == nil {
					t.Fatal("actual image environment bypassed rendered configuration validation")
				}
				for _, call := range fake.calls {
					if call[0] == "compose" && call[len(composePrefix(cfg))] != "config" {
						t.Fatal("unsafe override touched writers before rejection")
					}
				}
			})
		}
	}
}
