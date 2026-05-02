package orm

import "time"

type Config struct {
	Host          string     `mapstructure:"host"           env:"DB_HOST"           validate:"required,hostname|ip"`
	Port          string     `mapstructure:"port"           env:"DB_PORT"           validate:"required,numeric,gte=1,lte=65535"`
	User          string     `mapstructure:"user"           env:"DB_USER"           validate:"required"`
	Password      string     `mapstructure:"password"       env:"DB_PASSWORD"       validate:"required"`
	Database      string     `mapstructure:"database"       env:"DB_DATABASE"       validate:"required"`
	Insecure      bool       `mapstructure:"insecure"       env:"DB_INSECURE"`
	RunMigrations bool       `mapstructure:"run_migrations" env:"DB_RUN_MIGRATIONS"`
	RunSeeders    bool       `mapstructure:"run_seeders"    env:"DB_RUN_SEEDERS"`
	Pool          PoolConfig `mapstructure:"pool"`
}

type PoolConfig struct {
	MaxConns        int32         `mapstructure:"max_conns"          validate:"omitempty,gte=1"`
	MinConns        int32         `mapstructure:"min_conns"          validate:"omitempty,gte=0,ltefield=MaxConns"`
	MaxConnLifetime time.Duration `mapstructure:"max_conn_life_time" validate:"omitempty,gt=0"`
	MaxConnIdleTime time.Duration `mapstructure:"max_conn_idle_time" validate:"omitempty,gt=0"`
}
