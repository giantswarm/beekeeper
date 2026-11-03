// Package remotebuild runs read-only build commands in reusable, worktree-owned
// builder Jobs. Cluster names, images and credentials belong to desk configuration.
package remotebuild

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Config is opt-in; no Kubernetes call is made while Enabled is false.
type Config struct {
	Enabled          bool              `yaml:"enabled"`
	Kubeconfig       string            `yaml:"kubeconfig"`
	Context          string            `yaml:"context"`
	Namespace        string            `yaml:"namespace"`
	Images           map[string]string `yaml:"images"`
	CPU              string            `yaml:"cpu"`
	Memory           string            `yaml:"memory"`
	MaxPods          int               `yaml:"maxPods"`
	PlacementTimeout time.Duration     `yaml:"placementTimeout"`
	IdleTimeout      time.Duration     `yaml:"idleTimeout"`
	Cache            Cache             `yaml:"cache"`
}

// Defaults returns operational defaults without choosing a cluster or image.
func (c Config) Defaults() Config {
	if c.CPU == "" {
		c.CPU = "2"
	}
	if c.Memory == "" {
		c.Memory = "4Gi"
	}
	if c.MaxPods == 0 {
		c.MaxPods = 8
	}
	if c.PlacementTimeout == 0 {
		c.PlacementTimeout = time.Minute
	}
	if c.IdleTimeout == 0 {
		c.IdleTimeout = time.Hour
	}
	c.Cache = c.Cache.defaults()
	return c
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	c = c.Defaults()
	if err := c.Cache.validate(); err != nil {
		return err
	}
	if c.Kubeconfig == "" || c.Context == "" || len(validation.IsDNS1123Label(c.Namespace)) != 0 {
		return errors.New("run.remote requires explicit kubeconfig, context and a valid namespace")
	}
	if c.MaxPods < 1 || c.PlacementTimeout < time.Second || c.IdleTimeout < time.Minute {
		return errors.New("run.remote requires positive maxPods, placementTimeout >= 1s and idleTimeout >= 1m")
	}
	for key, value := range map[string]string{"cpu": c.CPU, "memory": c.Memory} {
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() <= 0 {
			return fmt.Errorf("run.remote.%s must be a positive resource quantity", key)
		}
	}
	if len(c.Images) == 0 {
		return errors.New("run.remote.images requires at least one toolchain image")
	}
	for tool, image := range c.Images {
		if (tool != "go" && tool != toolCargo) || image == "" {
			return errors.New("run.remote.images keys must be go or cargo, with nonempty images")
		}
	}
	return nil
}
