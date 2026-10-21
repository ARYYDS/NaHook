// Package deploy_test guards the packaging fixtures under deploy/: the sample
// configuration must be loadable and valid for the same parser the process
// uses (design §5; T8).
package deploy_test

import (
	"slices"
	"testing"

	"github.com/ARYYDS/NaHook/internal/config"
)

func TestConfigExampleIsValid(t *testing.T) {
	c, err := config.Load("config.example.json")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if c.Listen != "127.0.0.1:19080" {
		t.Errorf("listen = %q, want 127.0.0.1:19080", c.Listen)
	}
	if c.ControlSocket != "/run/nahook/ctl.sock" {
		t.Errorf("control_socket = %q, want /run/nahook/ctl.sock", c.ControlSocket)
	}
	if c.NewAPI != "http://127.0.0.1:3000" {
		t.Errorf("newapi = %q, want http://127.0.0.1:3000", c.NewAPI)
	}
	if c.BodyLimit != 67108864 {
		t.Errorf("body_limit = %d, want 67108864", c.BodyLimit)
	}
	if want := []string{"/v1/responses", "/v1/chat/completions"}; !slices.Equal(c.Match.Paths, want) {
		t.Errorf("match.paths = %v, want %v", c.Match.Paths, want)
	}
	if want := []string{"gpt-6-astra"}; !slices.Equal(c.Match.Models, want) {
		t.Errorf("match.models = %v, want %v", c.Match.Models, want)
	}
	if c.Retry.Max != 3 {
		t.Errorf("retry.max = %d, want 3", c.Retry.Max)
	}
	if len(c.Retry.PerModel) != 0 {
		t.Errorf("retry.per_model = %v, want empty", c.Retry.PerModel)
	}
	if c.Gate.BaseFloorMS != 50 {
		t.Errorf("gate.base_floor_ms = %d, want 50", c.Gate.BaseFloorMS)
	}
	if c.Gate.StreakCap != 3 {
		t.Errorf("gate.streak_cap = %d, want 3", c.Gate.StreakCap)
	}
}
