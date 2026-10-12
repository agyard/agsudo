//go:build !dev

package helper

func loadConfig() (Config, error) { return loadProduction() }
