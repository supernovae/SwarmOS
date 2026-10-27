package harness

import (
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

// Sharing host dependency caches and allowing private Go modules are node
// profile choices. A repository's .swarm/config.yaml must not be able to turn
// either on or widen it.
func TestConfigCannotEnableSharedHostCaches(t *testing.T) {
	valid, err := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []struct{ key, value string }{
		{"share_host_dependency_caches", " true"},
		{"private_go_modules", "\n    acme/a: ['*']"},
	} {
		line := setting.key + ":" + setting.value + "\n"
		for name, data := range map[string]string{
			"top level": string(valid) + strings.ReplaceAll(line, "\n    ", "\n  "),
			"execution": strings.Replace(string(valid), "execution:\n", "execution:\n  "+line, 1),
			"security":  strings.Replace(string(valid), "security:\n", "security:\n  "+line, 1),
		} {
			t.Run(setting.key+"/"+name, func(t *testing.T) {
				if data == string(valid) {
					t.Fatal("fixture did not change the template")
				}
				cfg, err := decodeConfig([]byte(data))
				if err == nil || cfg != nil || !strings.Contains(err.Error(), setting.key) {
					t.Fatalf("config set %s: cfg=%v err=%v", setting.key, cfg, err)
				}
			})
		}
	}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			field := typ.Field(i)
			name := strings.ToLower(field.Name + " " + field.Tag.Get("yaml"))
			if strings.Contains(name, "dependency") || strings.Contains(name, "host_cache") || strings.Contains(name, "hostcache") ||
				strings.Contains(name, "private") || strings.Contains(name, "module") {
				t.Errorf("SwarmConfig field %s.%s can select dependency caches", path, field.Name)
			}
			walk(field.Type, path+"."+field.Name)
		}
	}
	walk(reflect.TypeOf(contract.SwarmConfig{}), "SwarmConfig")
}
