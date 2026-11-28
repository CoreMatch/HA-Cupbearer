package config

import "testing"

func TestMigrateConfigV3ToV4(t *testing.T) {
	cfg := map[string]interface{}{"version": "3", "orchestration": map[string]interface{}{"enabled": true}}
	migrated, changed, err := MigrateConfig(cfg, nil)
	if err != nil || !changed || migrated["version"] != ConfigVersion {
		t.Fatalf("migration failed: migrated=%v changed=%v err=%v", migrated, changed, err)
	}
	if _, exists := migrated["orchestration"].(map[string]interface{})["deployment_roots"]; exists {
		t.Fatal("migration must leave discovery disabled by default")
	}
}
