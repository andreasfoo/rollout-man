// Package casedef reads the Harbor task.toml that ships inside a case.
package casedef

import (
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Resources struct {
	CPUs      int `toml:"cpus"`
	MemoryMB  int `toml:"memory_mb"`
	StorageMB int `toml:"storage_mb"`
	GPUs      int `toml:"gpus"`
}

type TaskConfig struct {
	Name        string
	Description string
	Category    string
	Difficulty  string

	Resources Resources

	AgentTimeout    time.Duration
	VerifierTimeout time.Duration
	BuildTimeout    time.Duration
	AgentUser       string
	VerifierUser    string
	AllowInternet   bool
}

type rawFile struct {
	SchemaVersion string `toml:"schema_version"`
	Task          struct {
		Name        string `toml:"name"`
		Description string `toml:"description"`
	} `toml:"task"`
	Metadata struct {
		Category   string `toml:"category"`
		Difficulty string `toml:"difficulty"`
	} `toml:"metadata"`
	Agent struct {
		TimeoutSec float64 `toml:"timeout_sec"`
		User       string  `toml:"user"`
	} `toml:"agent"`
	Verifier struct {
		TimeoutSec float64 `toml:"timeout_sec"`
		User       string  `toml:"user"`
	} `toml:"verifier"`
	Environment struct {
		BuildTimeoutSec float64 `toml:"build_timeout_sec"`
		CPUs            int     `toml:"cpus"`
		MemoryMB        int     `toml:"memory_mb"`
		StorageMB       int     `toml:"storage_mb"`
		GPUs            int     `toml:"gpus"`
		AllowInternet   bool    `toml:"allow_internet"`
	} `toml:"environment"`
}

func Load(caseDir string) (*TaskConfig, error) {
	b, err := os.ReadFile(filepath.Join(caseDir, "task.toml"))
	if err != nil {
		return nil, err
	}
	var r rawFile
	if err := toml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	secs := func(f float64, def time.Duration) time.Duration {
		if f <= 0 {
			return def
		}
		return time.Duration(f * float64(time.Second))
	}
	str := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	return &TaskConfig{
		Name:        r.Task.Name,
		Description: r.Task.Description,
		Category:    r.Metadata.Category,
		Difficulty:  r.Metadata.Difficulty,
		Resources: Resources{
			CPUs: r.Environment.CPUs, MemoryMB: r.Environment.MemoryMB,
			StorageMB: r.Environment.StorageMB, GPUs: r.Environment.GPUs,
		},
		AgentTimeout:    secs(r.Agent.TimeoutSec, time.Hour),
		VerifierTimeout: secs(r.Verifier.TimeoutSec, 5*time.Minute),
		BuildTimeout:    secs(r.Environment.BuildTimeoutSec, 30*time.Minute),
		AgentUser:       str(r.Agent.User, "agent"),
		VerifierUser:    str(r.Verifier.User, "root"),
		AllowInternet:   r.Environment.AllowInternet,
	}, nil
}
