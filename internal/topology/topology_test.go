package topology

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBuildReadsLocalsAndManifests(t *testing.T) {
	cfg := Config{
		Vars:        map[string]string{"env": "prod"},
		Services:    Local{Dir: "infra", Local: "services"},
		Access:      &Local{Dir: "infra", Local: "access"},
		Manifests:   []string{"services/*/prod.yaml"},
		ManifestEnv: map[string]string{"APP_MODE": "mode"},
	}
	m, err := Build(cfg, "testdata/repo", time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, s := range m.Services {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"checkout", "web"}) {
		t.Fatalf("services = %v, want [checkout web]", names)
	}
	svc := m.Services[0]
	if svc.Repo != "shop" || svc.Ingress != "internal" || svc.Scaling != "1–10 instances" {
		t.Errorf("repo/ingress/scaling = %q/%q/%q", svc.Repo, svc.Ingress, svc.Scaling)
	}
	if !slices.Equal(svc.Spanner, []string{"orders"}) || !slices.Equal(svc.Publishes, []string{"orders-prod"}) {
		t.Errorf("access = spanner %v publishes %v", svc.Spanner, svc.Publishes)
	}
	if len(svc.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(svc.Containers))
	}
	c := svc.Containers[0]
	if c.OTelName != "checkout-api" || !c.Serving {
		t.Errorf("otel %q serving %v", c.OTelName, c.Serving)
	}
	if !slices.Equal(c.Env, []EnvFact{{Label: "mode", Value: "api"}}) {
		t.Errorf("env = %v", c.Env)
	}
	if !slices.Equal(c.Secrets, []string{"DB_PASSWORD"}) {
		t.Errorf("secrets = %v", c.Secrets)
	}
	if !slices.Equal(c.Calls, []string{"PAYMENTS_URL=https://payments.example.com/v1"}) {
		t.Errorf("calls = %v", c.Calls)
	}
	if out := Render(m); !strings.Contains(out, "mode api") {
		t.Errorf("rendered map lacks the manifestEnv fact:\n%s", out)
	}
}

func TestBuildFailsWithoutServices(t *testing.T) {
	_, err := Build(Config{Services: Local{Dir: "missing", Local: "services"}}, "testdata/repo", time.Now())
	if err == nil {
		t.Fatal("want an error for a services anchor that does not exist")
	}
}
