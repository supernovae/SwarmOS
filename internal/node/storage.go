package node

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/supernovae/swarmos/internal/contract"
	"gopkg.in/yaml.v3"
)

// StorageSettings contains validated operator values, never repository policy.
type StorageSettings struct {
	CacheTargetBytes, LowFreeBytes                          int64
	LowFreePercent                                          float64
	InactiveRetention, MaintenanceInterval, WarningInterval time.Duration
}

func DefaultStorageSettings() StorageSettings {
	return StorageSettings{40 << 30, 5 << 30, 10, 14 * 24 * time.Hour, 30 * time.Minute, 10 * time.Minute}
}

// LoadStorageSettings reads only the explicitly selected operator profile.
func LoadStorageSettings(path string) (StorageSettings, error) {
	out := DefaultStorageSettings()
	if path == "" {
		path = DefaultNodeConfigPath()
	}
	if !filepath.IsAbs(path) {
		return out, fmt.Errorf("operator storage profile path must be absolute")
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var doc struct {
		Storage yaml.Node `yaml:"storage"`
	}
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return out, err
	}
	if doc.Storage.Kind == 0 {
		return out, nil
	}
	raw, err := yaml.Marshal(&doc.Storage)
	if err != nil {
		return out, err
	}
	var cfg contract.NodeStorage
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err = dec.Decode(&cfg); err != nil {
		return out, fmt.Errorf("storage settings: %w", err)
	}
	for _, x := range []struct {
		src *int64
		dst *int64
	}{{cfg.CacheTargetBytes, &out.CacheTargetBytes}, {cfg.LowFreeBytes, &out.LowFreeBytes}} {
		if x.src != nil {
			if *x.src <= 0 {
				return out, fmt.Errorf("storage bytes must be positive")
			}
			*x.dst = *x.src
		}
	}
	if cfg.LowFreePercent != nil {
		v := *cfg.LowFreePercent
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 100 {
			return out, fmt.Errorf("storage percentage must be in (0,100]")
		}
		out.LowFreePercent = v
	}
	for _, x := range []struct {
		src *string
		dst *time.Duration
	}{{cfg.InactiveRetention, &out.InactiveRetention}, {cfg.MaintenanceInterval, &out.MaintenanceInterval}, {cfg.WarningInterval, &out.WarningInterval}} {
		if x.src != nil {
			v, e := time.ParseDuration(*x.src)
			if e != nil || v <= 0 {
				return out, fmt.Errorf("storage duration must be positive and representable")
			}
			*x.dst = v
		}
	}
	return out, nil
}

// decodeExecutionProfile leaves storage validation to LoadStorageSettings so a
// broken maintenance setting cannot prevent otherwise valid work from running.
func decodeExecutionProfile(data []byte, prof *contract.NodeProfile) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return doc.Decode(prof)
	}
	mapping := doc.Content[0]
	var storageNode *yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "storage" {
			storageNode = mapping.Content[i+1]
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			break
		}
	}
	if err := doc.Decode(prof); err != nil {
		return err
	}
	if storageNode != nil {
		var cfg contract.NodeStorage
		if storageNode.Decode(&cfg) == nil {
			prof.Storage = &cfg
		}
	}
	return nil
}
