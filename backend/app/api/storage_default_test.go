// Tofu (2026-11-05): tests for the storage default this fork adds.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/sysadminsmedia/homebox/backend/internal/sys/config"
	"github.com/sysadminsmedia/homebox/backend/internal/sys/postgresblob"
)

func TestApplyDefaultStorage(t *testing.T) {
	tests := []struct {
		name   string
		driver string
		env    string
		config string
		want   string
	}{
		{
			name:   "postgres uses the database",
			driver: config.DriverPostgres,
			config: "file:///./",
			want:   postgresblob.Scheme + "://",
		},
		{
			name:   "an owner's environment value wins",
			driver: config.DriverPostgres,
			env:    "s3://my-bucket?region=eu-west-1",
			// config.New has already copied the environment value into the
			// config by the time run() calls applyDefaultStorage.
			config: "s3://my-bucket?region=eu-west-1",
			want:   "s3://my-bucket?region=eu-west-1",
		},
		{
			// config.New has copied --storage-conn-string into the config.
			name:   "an owner's command-line value wins",
			driver: config.DriverPostgres,
			config: "file:///var/lib/homebox",
			want:   "file:///var/lib/homebox",
		},
		{
			name:   "sqlite keeps file storage",
			driver: config.DriverSqlite3,
			config: "file:///./",
			want:   "file:///./",
		},
		{
			// setupDatabase reads the driver in lower case, so this one is on
			// PostgreSQL too.
			name:   "postgres in capitals uses the database",
			driver: "Postgres",
			config: "file:///./",
			want:   postgresblob.Scheme + "://",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HBOX_STORAGE_CONN_STRING", tt.env)
			cfg := &config.Config{
				Storage:  config.Storage{ConnString: tt.config},
				Database: config.Database{Driver: tt.driver},
			}
			applyDefaultStorage(cfg)
			assert.Equal(t, tt.want, cfg.Storage.ConnString)
		})
	}
}
