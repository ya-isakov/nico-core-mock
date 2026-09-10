package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExampleConfig(t *testing.T) {
	path := filepath.Join("..", "..", "helm", "nico-rest-mock-core", "values.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skip("example config not found")
	}

	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(inv.Machines) != 6 {
		t.Fatalf("expected 6 machines, got %d", len(inv.Machines))
	}
	if len(inv.ExpectedMachines) != 0 {
		t.Fatalf("expected 0 expected machines, got %d", len(inv.ExpectedMachines))
	}

	for id, machine := range inv.Machines {
		if machine.DiscoveryInfo == nil {
			t.Fatalf("machine %q: discovery info not loaded", id)
		}
		if len(machine.DiscoveryInfo.GetNetworkInterfaces()) == 0 {
			t.Fatalf("machine %q: discovery info has no network interfaces", id)
		}
	}
}
