package harness

import (
	"reflect"
	"strings"
	"testing"

	"github.com/supernovae/swarmos/internal/contract"
)

// Sharing host dependency caches is a node profile choice. A repository's
// .swarm/config.yaml must not be able to turn it on.
func TestConfigCannotEnableSharedHostCaches(t *testing.T) {
	valid, err := embeddedTemplates.ReadFile("templates/swarm/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"top level": string(valid) + "share_host_dependency_caches: true\n",
		"execution": strings.Replace(string(valid), "execution:\n", "execution:\n  share_host_dependency_caches: true\n", 1),
		"security":  strings.Replace(string(valid), "security:\n", "security:\n  share_host_dependency_caches: true\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if data == string(valid) {
				t.Fatal("fixture did not change the template")
			}
			cfg, err := decodeConfig([]byte(data))
			if err == nil || cfg != nil || !strings.Contains(err.Error(), "share_host_dependency_caches") {
				t.Fatalf("config enabled shared host caches: cfg=%v err=%v", cfg, err)
			}
		})
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
			if strings.Contains(name, "dependency") || strings.Contains(name, "host_cache") || strings.Contains(name, "hostcache") {
				t.Errorf("SwarmConfig field %s.%s can select dependency caches", path, field.Name)
			}
			walk(field.Type, path+"."+field.Name)
		}
	}
	walk(reflect.TypeOf(contract.SwarmConfig{}), "SwarmConfig")
}
