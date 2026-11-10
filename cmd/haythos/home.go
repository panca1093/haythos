package main

import (
	"errors"
	"fmt"
	"path/filepath"
)

type paths struct {
	Home, Run, Socket, PID, SpawnLock, Logs string
}

// homePaths roots everything at $HAYTHOS_HOME, else ~/.haythos.
func homePaths(getenv func(string) string) (paths, error) {
	home := getenv("HAYTHOS_HOME")
	if home == "" {
		user := getenv("HOME")
		if user == "" {
			return paths{}, errors.New("neither HAYTHOS_HOME nor HOME is set")
		}
		home = filepath.Join(user, ".haythos")
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return paths{}, fmt.Errorf("resolve haythos home: %w", err)
	}
	run := filepath.Join(home, "run")
	return paths{
		Home:      home,
		Run:       run,
		Socket:    filepath.Join(run, "haythos.sock"),
		PID:       filepath.Join(run, "haythos.pid"),
		SpawnLock: filepath.Join(run, "spawn.lock"),
		Logs:      filepath.Join(home, "logs"),
	}, nil
}
