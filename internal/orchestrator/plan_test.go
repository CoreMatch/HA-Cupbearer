package orchestrator

import (
	"reflect"
	"testing"

	"HA-Cupbearer/internal/manifest"
)

func planManifest(t *testing.T) []*manifest.Manifest {
	t.Helper()
	// 依赖图：core(hrpauth) <- satellite(ygg, skin) <- satellite(winner 依赖 ygg)；
	// ops(mv 依赖 deployer)；deployer 无依赖。
	ms := []*manifest.Manifest{
		{Project: "winner", Group: "satellite", Facts: manifest.Facts{DependsOn: []manifest.Dependency{{Project: "ygg", Kind: "required"}, {Project: "hrpauth", Kind: "required"}}}},
		{Project: "ygg", Group: "satellite", Facts: manifest.Facts{DependsOn: []manifest.Dependency{{Project: "hrpauth", Kind: "required"}}}},
		{Project: "skin", Group: "satellite", Facts: manifest.Facts{DependsOn: []manifest.Dependency{{Project: "hrpauth", Kind: "required"}}}},
		{Project: "hrpauth", Group: "core"},
		{Project: "deployer", Group: "ops"},
		{Project: "mv", Group: "ops", Facts: manifest.Facts{DependsOn: []manifest.Dependency{{Project: "deployer", Kind: "required"}}}},
		{Project: "mysql", Kind: "external", Group: "infra"},
	}
	return ms
}

func TestPlan_FullOrder(t *testing.T) {
	order, err := Plan(planManifest(t), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// 约束：mysql(infra) 最前；hrpauth 先于三个卫星；ygg 先于 winner；deployer 先于 mv。
	pos := make(map[string]int)
	for i, name := range order {
		pos[name] = i
	}
	checks := [][2]string{
		{"mysql", "hrpauth"},
		{"hrpauth", "ygg"},
		{"hrpauth", "skin"},
		{"ygg", "winner"},
		{"deployer", "mv"},
	}
	for _, c := range checks {
		if pos[c[0]] >= pos[c[1]] {
			t.Errorf("expect %s before %s, got order %v", c[0], c[1], order)
		}
	}
	if len(order) != 7 {
		t.Errorf("expect 7 entries, got %v", order)
	}
}

func TestPlan_ScopeFilter(t *testing.T) {
	order, err := Plan(planManifest(t), []string{"winner", "ygg", "hrpauth"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hrpauth", "ygg", "winner"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestPlan_UnknownProject(t *testing.T) {
	if _, err := Plan(planManifest(t), []string{"ghost"}, ""); err == nil {
		t.Error("expect error for unknown project")
	}
}

func TestRequiredDependents(t *testing.T) {
	got := requiredDependents(planManifest(t), "hrpauth")
	want := []string{"skin", "winner", "ygg"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("requiredDependents = %v, want %v", got, want)
	}
}
