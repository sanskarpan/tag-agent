// Package cli implements the TAG command tree (Go port of src/tag/cmd/*.py).
package cli

import (
	"github.com/tag-agent/tag/internal/config"
	"github.com/tag-agent/tag/internal/store"
)

// App is the shared per-invocation context (config + DB).
type App struct {
	ConfigPath string
	Cfg        *config.Config
	DB         *store.DB
}

// Load resolves + loads config for the given override path.
func (a *App) Load(override string) error {
	p, err := config.Path(override)
	if err != nil {
		return err
	}
	a.ConfigPath = p
	a.Cfg, err = config.Load(p)
	return err
}

// OpenDB opens the runtime store (lazy).
func (a *App) OpenDB() (*store.DB, error) {
	if a.DB != nil {
		return a.DB, nil
	}
	db, err := store.Open(a.Cfg)
	if err != nil {
		return nil, err
	}
	a.DB = db
	return db, nil
}
